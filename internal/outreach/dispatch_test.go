package outreach_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/entitlements"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/privacy"
)

// ---- transaction probe

// probeTx is a TxManager that numbers its outermost transactions and
// carries the number in ctx (db.NopTx underneath: outbox.Mem records on
// commit only).
type probeTx struct {
	mu sync.Mutex
	n  int
}

type probeKey struct{}

func (p *probeTx) WithTx(ctx context.Context, fn func(context.Context) error) error {
	if _, ok := ctx.Value(probeKey{}).(int); ok {
		return fn(ctx) // join
	}
	p.mu.Lock()
	p.n++
	id := p.n
	p.mu.Unlock()
	return db.NopTx{}.WithTx(context.WithValue(ctx, probeKey{}, id), fn)
}

func (p *probeTx) WithSerializableTx(ctx context.Context, fn func(context.Context) error) error {
	return p.WithTx(ctx, fn)
}

// txSeen records the transaction of each write.
type txSeen struct {
	mu   sync.Mutex
	seen map[string][]int
}

func (s *txSeen) mark(ctx context.Context, what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, _ := ctx.Value(probeKey{}).(int)
	s.seen[what] = append(s.seen[what], id)
}

type seenEnts struct {
	outreach.Entitlements
	s *txSeen
}

func (e seenEnts) Consume(ctx context.Context, org uuid.UUID, f entitlements.Feature, n int, ref entitlements.Ref) (uuid.UUID, error) {
	e.s.mark(ctx, "consume")
	return e.Entitlements.Consume(ctx, org, f, n, ref)
}

type seenStore struct {
	outreach.Store
	s *txSeen
}

func (st seenStore) InsertDispatch(ctx context.Context, d outreach.Dispatch) (outreach.Dispatch, error) {
	st.s.mark(ctx, "insert")
	return st.Store.InsertDispatch(ctx, d)
}

type seenAudit struct {
	inner outreach.AuditLogger
	s     *txSeen
}

func (a seenAudit) Log(ctx context.Context, e audit.Event) error {
	a.s.mark(ctx, "audit:"+e.Action)
	return a.inner.Log(ctx, e)
}

type seenOutbox struct {
	inner interface {
		Add(context.Context, domainevent.Envelope) error
		AfterCommit(context.Context, domainevent.Envelope) error
	}
	s *txSeen
}

func (o seenOutbox) Add(ctx context.Context, e domainevent.Envelope) error {
	o.s.mark(ctx, "outbox:"+e.Topic)
	return o.inner.Add(ctx, e)
}

func (o seenOutbox) AfterCommit(ctx context.Context, e domainevent.Envelope) error {
	return o.inner.AfterCommit(ctx, e)
}

// TestScheduleDispatch_ConsumesInSameTx (01 §6.3): a company's
// targeted_dispatch unit, the dispatch row, outreach.dispatch_scheduled and
// the audit record are written in ONE transaction; called inside the
// caller's transaction (the feed's targeted post), ScheduleDispatch joins
// it instead of opening its own. (PostgreSQL's rollback of the unit:
// TestScheduleDispatch_Postgres_RollbackReleasesUnit.)
func TestScheduleDispatch_ConsumesInSameTx(t *testing.T) {
	t.Parallel()
	seen := &txSeen{seen: map[string][]int{}}
	tx := &probeTx{}
	e := newEnv(t, func(e *env, d *outreach.Deps) {
		d.Tx = tx
		d.Store = seenStore{Store: d.Store, s: seen}
		d.Entitlements = seenEnts{Entitlements: d.Entitlements, s: seen}
		d.Audit = seenAudit{inner: d.Audit, s: seen}
		d.Outbox = seenOutbox{inner: e.out, s: seen}
	})
	e.grantDispatches(t, companyA, 2)
	p := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))

	if _, err := e.svc.ScheduleDispatch(t.Context(), outreach.DispatchInput{Actor: actor(t, "company_admin"), Post: p,
		Channels: []outreach.Channel{outreach.ChannelPush}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"consume", "insert", "outbox:" + domainevent.TopicOutreachDispatchScheduled,
		"audit:" + string(authz.OutreachDispatchCreate)}
	var txs []int
	for _, w := range want {
		got := seen.seen[w]
		if len(got) != 1 || got[0] == 0 {
			t.Fatalf("%s seen in transactions %v, want exactly one", w, got)
		}
		txs = append(txs, got[0])
	}
	if uniq := slices.Compact(slices.Clone(txs)); len(uniq) != 1 || tx.n != 1 {
		t.Fatalf("writes in transactions %v (%d opened), want one", txs, tx.n)
	}
	if e.units(t, companyA) != 1 {
		t.Fatalf("units left = %d, want 1", e.units(t, companyA))
	}

	// Inside the caller's transaction it opens none of its own.
	p2 := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	err := tx.WithTx(t.Context(), func(ctx context.Context) error {
		_, err := e.svc.ScheduleDispatch(ctx, outreach.DispatchInput{Actor: actor(t, "company_admin"), Post: p2,
			Channels: []outreach.Channel{outreach.ChannelPush}})
		return err
	})
	if err != nil || tx.n != 2 || seen.seen["consume"][1] != 2 || seen.seen["insert"][1] != 2 {
		t.Fatalf("joined: err %v, opened %d, consume %v, insert %v", err, tx.n, seen.seen["consume"], seen.seen["insert"])
	}
}

// TestScheduleDispatch_IdempotencyReplay_NoDoubleConsume: createDispatch
// with the same Idempotency-Key returns the same dispatch without a second
// unit, row or event; the same key with another body is 422; another key on
// the same post is 409 outreach.dispatch_exists and spends nothing.
func TestScheduleDispatch_IdempotencyReplay_NoDoubleConsume(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 5)
	p := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	cm := actor(t, "company_member")
	ctx := withKey(t.Context(), cm, "/api/v1/posts/{post_id}/dispatches", "dispatch-0001")
	req := outreach.DispatchRequest{Channels: []outreach.Channel{outreach.ChannelPush}}

	first, err := e.svc.CreateDispatch(ctx, cm, p.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.CreateDispatch(ctx, cm, p.ID, req)
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay = %s, %v (first %s)", again.ID, err, first.ID)
	}
	if e.units(t, companyA) != 4 {
		t.Fatalf("units left = %d after a replay, want 4", e.units(t, companyA))
	}
	n := 0
	for _, topic := range e.out.Topics() {
		if topic == domainevent.TopicOutreachDispatchScheduled {
			n++
		}
	}
	if n != 1 || len(e.audit.actionsOf(string(authz.OutreachDispatchCreate))) != 1 {
		t.Fatalf("events %d, audit %v", n, e.audit.actions())
	}
	_, err = e.svc.CreateDispatch(ctx, cm, p.ID, outreach.DispatchRequest{Channels: []outreach.Channel{outreach.ChannelEmail}})
	if status, code := problemOf(err); status != 422 || code != "idempotency.key_reused" {
		t.Fatalf("same key, other body = %v", err)
	}
	_, err = e.svc.CreateDispatch(withKey(t.Context(), cm, "/api/v1/posts/{post_id}/dispatches", "dispatch-0002"), cm, p.ID, req)
	if status, code := problemOf(err); status != 409 || code != "outreach.dispatch_exists" {
		t.Fatalf("second dispatch of the post = %v", err)
	}
	if e.units(t, companyA) != 4 {
		t.Fatalf("units left = %d after a refused dispatch, want 4", e.units(t, companyA))
	}
}

// TestRunDispatch_CommercialOnlyConsented (6563, İYS; 03 k18): a company's
// dispatch reaches a student on a channel only with that channel's
// commercial consent in force (push: commercial_push, e-mail:
// commercial_email); everyone else gets nothing — notify is not even
// called for them. The messages are commercial, carry no personal data in
// the push data, and the e-mail carries the one-click unsubscribe link.
func TestRunDispatch_CommercialOnlyConsented(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 1)
	ids := e.audience.add(30, true, uniKey(uniA))
	e.users.add(false, ids...)
	pushOK, mailOK := ids[:12], ids[10:17] // two students with both
	e.consents.grant(consent.PurposeCommercialPush, pushOK...)
	e.consents.grant(consent.PurposeCommercialEmail, mailOK...)
	e.consents.grant(consent.PurposeDiscoveryProfile, ids[20:]...) // another purpose is no commercial consent
	p := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	d := e.schedule(t, "company_member", p, outreach.ChannelPush, outreach.ChannelEmail)

	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	if got := sorted(e.sender.to(notify.ChannelPush)); !slices.Equal(got, sorted(pushOK)) {
		t.Fatalf("push to %d students, want the %d consenting", len(got), len(pushOK))
	}
	if got := sorted(e.sender.to(notify.ChannelEmail)); !slices.Equal(got, sorted(mailOK)) {
		t.Fatalf("e-mail to %d students, want the %d consenting", len(got), len(mailOK))
	}
	if e.sender.attempts != len(pushOK)+len(mailOK) {
		t.Fatalf("notify called %d times, want %d (never for a student without consent)", e.sender.attempts,
			len(pushOK)+len(mailOK))
	}
	for _, m := range e.sender.messages() {
		switch {
		case m.Category != notify.CategoryCommercial || m.Template != "outreach.commercial":
			t.Fatalf("message %s/%s", m.Category, m.Template)
		case m.DedupeKey != "dispatch:"+d.ID.String()+":"+m.UserID.String()+":"+string(m.Channel):
			t.Fatalf("dedupe key %q", m.DedupeKey)
		case m.LinkPath != "/posts/"+p.ID.String():
			t.Fatalf("link %q", m.LinkPath)
		case m.Channel == notify.ChannelEmail && (m.UnsubscribeURL != testUnsubscribe || m.ToEmail == ""):
			t.Fatalf("commercial e-mail without the unsubscribe link: %+v", m)
		case m.Channel == notify.ChannelPush:
			for k, v := range m.Data {
				if _, ok := v.(uuid.UUID); !ok {
					t.Fatalf("push data %s=%v is not an opaque ID", k, v)
				}
			}
		}
	}
	got := e.dispatch(t, d)
	switch {
	case got.Status != outreach.DispatchCompleted || got.CompletedAt == nil || got.StartedAt == nil:
		t.Fatalf("dispatch %s", got.Status)
	case !eqInt(got.EligibleCount, 30) || !eqInt(got.ConsentedCount, 10) || !eqInt(got.SentCount, 10):
		// 30 eligible; 17 consenting and notified → bucket 10
		t.Fatalf("counts %v/%v/%v", deref(got.EligibleCount), deref(got.ConsentedCount), deref(got.SentCount))
	}
	if acts := e.audit.actionsOf(string(authz.OutreachDispatchRun)); len(acts) != 1 {
		t.Fatalf("run audit = %v", acts)
	}
}

// TestRunDispatch_ClubInformational_NoConsentNeeded (T-16): a club's
// notice to its own members is informational: neither consent nor a unit,
// no k ≥ 10 (4 members), the member list instead of the audience walk, the
// informational template.
func TestRunDispatch_ClubInformational_NoConsentNeeded(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	members := []uuid.UUID{uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()}
	e.users.add(false, members...)
	e.members.set(clubA, members...)
	p := e.targetedPost(clubA, outreach.AuthorClub, clubKey(clubA))
	d := e.schedule(t, "club_admin", p, outreach.ChannelInApp, outreach.ChannelPush)
	if d.Commercial || d.EntitlementUsageID != nil {
		t.Fatalf("club notice commercial %v, unit %v", d.Commercial, d.EntitlementUsageID)
	}
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []notify.Channel{notify.ChannelInApp, notify.ChannelPush} {
		if got := sorted(e.sender.to(ch)); !slices.Equal(got, sorted(members)) {
			t.Fatalf("%s to %d members, want 4", ch, len(got))
		}
	}
	for _, m := range e.sender.messages() {
		if m.Category != notify.CategoryTransactional || m.Template != "outreach.announcement" {
			t.Fatalf("message %s/%s", m.Category, m.Template)
		}
		if m.Channel == notify.ChannelInApp && (m.SourceOrgID == nil || *m.SourceOrgID != clubA) {
			t.Fatalf("in-app source org %v", m.SourceOrgID)
		}
	}
	if e.consents.callCount() != 0 || e.audience.streamCount() != 0 {
		t.Fatalf("consent asked %d times, audience walked %d times", e.consents.callCount(), e.audience.streamCount())
	}
	got := e.dispatch(t, d)
	if got.Status != outreach.DispatchCompleted || got.EligibleCount != nil || got.SentCount != nil {
		t.Fatalf("dispatch %s, counts %v/%v (4 < 10: no number)", got.Status, deref(got.EligibleCount), deref(got.SentCount))
	}
	// A club_admin may only address its own members.
	other := e.targetedPost(clubA, outreach.AuthorClub, uniKey(uniA))
	_, err := e.svc.ScheduleDispatch(t.Context(), outreach.DispatchInput{Actor: actor(t, "club_admin"), Post: other,
		Channels: []outreach.Channel{outreach.ChannelInApp}})
	wantErr(t, err, outreach.ErrTargetOutOfScope)
}

// TestRunDispatch_ExcludesMinorsForCommercial: a student under 18 never
// receives a commercial dispatch, consent or not — even when students'
// eligibility flag is stale (every batch is checked against identity
// again, T-135); a suspended account neither.
func TestRunDispatch_ExcludesMinorsForCommercial(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 1)
	adults := e.audience.add(12, true, uniKey(uniA))
	minors := e.audience.add(3, true, uniKey(uniA)) // stale flag: identity knows better
	e.users.add(false, adults...)
	e.users.add(true, minors...)
	e.users.suspend(adults[0])
	all := append(slices.Clone(adults), minors...)
	e.consents.grant(consent.PurposeCommercialPush, all...)
	d := e.schedule(t, "company_admin", e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), outreach.ChannelPush)

	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	got := e.sender.to(notify.ChannelPush)
	for _, m := range minors {
		if slices.Contains(got, m) {
			t.Fatal("a minor received a commercial dispatch")
		}
	}
	if slices.Contains(got, adults[0]) || len(got) != 11 {
		t.Fatalf("pushed to %d students, want the 11 active adults", len(got))
	}
	if d = e.dispatch(t, d); !eqInt(d.EligibleCount, 10) || !eqInt(d.SentCount, 10) {
		t.Fatalf("counts %v/%v (11 adults → 10)", deref(d.EligibleCount), deref(d.SentCount))
	}
}

// TestRunDispatch_SensitiveClubMembers_MemberList (T-258, 03 Y2): a
// sensitive club's notice to its own members walks the member list (the
// audience never holds a sensitive club's key), skips minors, and keeps no
// count at all (a sensitive membership is never counted).
func TestRunDispatch_SensitiveClubMembers_MemberList(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	adults := make([]uuid.UUID, 12)
	for i := range adults {
		adults[i] = uuid.NewV7()
	}
	minor := uuid.NewV7()
	e.users.add(false, adults...)
	e.users.add(true, minor)
	e.members.set(clubSensitive, append(slices.Clone(adults), minor)...)
	p := e.targetedPost(clubSensitive, outreach.AuthorClub, clubKey(clubSensitive))
	d := e.schedule(t, "club_admin_sensitive", p, outreach.ChannelInApp)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	if got := sorted(e.sender.to(notify.ChannelInApp)); !slices.Equal(got, sorted(adults)) {
		t.Fatalf("notified %d, want the 12 adult members", len(got))
	}
	if e.audience.streamCount() != 0 {
		t.Fatal("a sensitive club's notice walked the audience")
	}
	got := e.dispatch(t, d)
	if got.Status != outreach.DispatchCompleted || got.EligibleCount != nil || got.ConsentedCount != nil || got.SentCount != nil {
		t.Fatalf("dispatch %s with counts %v/%v/%v: a sensitive membership is never counted", got.Status,
			deref(got.EligibleCount), deref(got.ConsentedCount), deref(got.SentCount))
	}
	// A company never dispatches to a sensitive club (03 Y2), even a post
	// compiled before the club became sensitive.
	e.grantDispatches(t, companyA, 1)
	cp := e.targetedPost(companyA, outreach.AuthorCompany, clubKey(clubSensitive))
	_, err := e.svc.ScheduleDispatch(t.Context(), outreach.DispatchInput{Actor: actor(t, "company_admin"), Post: cp,
		Channels: []outreach.Channel{outreach.ChannelPush}})
	wantErr(t, err, outreach.ErrSensitiveTarget)
}

// TestRunDispatch_RetrySafe_NoDuplicateDelivery: a run that breaks in the
// middle (notify down after 10 notifications) gives its claim back and is
// retried from the start; notify's dedupe keys skip what went out, so every
// student is notified exactly once; a third delivery of the event changes
// nothing.
func TestRunDispatch_RetrySafe_NoDuplicateDelivery(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ids := e.audience.add(25, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.sender.failAfter, e.sender.failErr = 10, errors.New("notify store unavailable")
	d := e.schedule(t, "university_rep", e.targetedPost(uniOrgA, outreach.AuthorUniversity, uniKey(uniA)), outreach.ChannelPush)

	if err := e.deliver(t); err == nil {
		t.Fatal("the broken run reported success")
	}
	if got := e.dispatch(t, d); got.Status != outreach.DispatchScheduled || len(e.sender.to(notify.ChannelPush)) != 10 {
		t.Fatalf("after the break: %s, %d notified", got.Status, len(e.sender.to(notify.ChannelPush)))
	}
	if err := e.deliver(t); err != nil { // the retry
		t.Fatal(err)
	}
	got := e.sender.to(notify.ChannelPush)
	if len(got) != 25 || len(slices.Compact(sorted(got))) != 25 {
		t.Fatalf("%d deliveries to %d students, want 25 to 25", len(got), len(slices.Compact(sorted(got))))
	}
	if d = e.dispatch(t, d); d.Status != outreach.DispatchCompleted || !eqInt(d.SentCount, 20) {
		t.Fatalf("dispatch %s sent %v", d.Status, deref(d.SentCount))
	}
	attempts := e.sender.attempts
	if err := e.run(t, d); err != nil || e.sender.attempts != attempts {
		t.Fatalf("a finished dispatch ran again: %v, %d attempts", err, e.sender.attempts-attempts)
	}
}

// TestCancelDispatch_ReleasesEntitlement: cancelling a scheduled dispatch
// gives the company's unit back in the same transaction; a second cancel
// changes nothing (no second release); the cancelled dispatch never runs;
// a finished dispatch cannot be cancelled (409); another org's is 404.
func TestCancelDispatch_ReleasesEntitlement(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 2)
	ids := e.audience.add(12, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.consents.grant(consent.PurposeCommercialPush, ids...)
	cma := actor(t, "company_admin")
	d := e.schedule(t, "company_member", e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), outreach.ChannelPush)
	if e.units(t, companyA) != 1 {
		t.Fatalf("units after scheduling = %d", e.units(t, companyA))
	}
	v, err := e.svc.CancelDispatch(t.Context(), cma, companyA, d.ID)
	if err != nil || v.Status != outreach.DispatchCancelled || v.CompletedAt == nil {
		t.Fatalf("cancel = %+v, %v", v.Dispatch, err)
	}
	if e.units(t, companyA) != 2 {
		t.Fatalf("units after cancel = %d, want 2", e.units(t, companyA))
	}
	if v, err := e.svc.CancelDispatch(t.Context(), cma, companyA, d.ID); err != nil || v.Status != outreach.DispatchCancelled ||
		e.units(t, companyA) != 2 {
		t.Fatalf("second cancel = %s, %v, units %d", v.Status, err, e.units(t, companyA))
	}
	if err := e.deliver(t); err != nil || e.sender.attempts != 0 {
		t.Fatalf("a cancelled dispatch ran: %v, %d sends", err, e.sender.attempts)
	}
	if acts := e.audit.actionsOf(string(authz.OutreachDispatchCancel)); len(acts) != 1 {
		t.Fatalf("cancel audit = %v", acts)
	}
	// A finished dispatch cannot be cancelled; another org's is not found.
	done := e.schedule(t, "company_member", e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), outreach.ChannelPush)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.CancelDispatch(t.Context(), cma, companyA, done.ID)
	wantErr(t, err, outreach.ErrInvalidState)
	_, err = e.svc.CancelDispatch(t.Context(), actor(t, "other_company_member"), companyB, done.ID)
	wantErr(t, err, outreach.ErrNotFound)
	if e.units(t, companyA) != 1 {
		t.Fatalf("units = %d: the completed dispatch keeps its unit", e.units(t, companyA))
	}
}

// TestGetDispatch_CountsNullBelow10 (06 H-61, T-260): the counts are
// published estimates only: null below 10, rounded down from 10 on, in the
// row as in the answer; a row holding an exact number (written outside the
// service) is still shown rounded.
func TestGetDispatch_CountsNullBelow10(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 1)
	ids := e.audience.add(23, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.consents.grant(consent.PurposeCommercialPush, ids[:7]...) // 7 consenting: below 10
	d := e.schedule(t, "company_admin", e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), outreach.ChannelPush)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	v, err := e.svc.GetDispatch(t.Context(), actor(t, "company_member"), companyA, d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !eqInt(v.Counts.Eligible, 20) || v.Counts.Consented != nil || v.Counts.Sent != nil {
		t.Fatalf("counts %v/%v/%v, want 20/null/null", deref(v.Counts.Eligible), deref(v.Counts.Consented), deref(v.Counts.Sent))
	}
	if !eqInt(v.EligibleCount, 20) || v.ConsentedCount != nil || v.SentCount != nil {
		t.Fatalf("stored counts %v/%v/%v: the exact numbers are never stored", deref(v.EligibleCount),
			deref(v.ConsentedCount), deref(v.SentCount))
	}
	dto := outreach.ToDispatchDTO(v)
	if dto.ConsentedCount != nil || dto.SentCount != nil || !eqInt(dto.EligibleCount, 20) {
		t.Fatalf("dto %+v", dto)
	}

	// An exact number in the row is rounded on the way out.
	mem, ok := e.store.(*outreach.MemStore)
	if !ok {
		t.Fatal("not the in-memory store")
	}
	raw := e.dispatch(t, d)
	raw.EligibleCount, raw.ConsentedCount, raw.SentCount = ptr(137), ptr(9), ptr(12)
	if err := mem.PutDispatch(raw); err != nil {
		t.Fatal(err)
	}
	v, err = e.svc.GetDispatch(t.Context(), actor(t, "platform_admin"), companyA, d.ID)
	if err != nil || !eqInt(v.Counts.Eligible, 100) || v.Counts.Consented != nil || !eqInt(v.Counts.Sent, 10) {
		t.Fatalf("rounded counts %v/%v/%v, %v", deref(v.Counts.Eligible), deref(v.Counts.Consented), deref(v.Counts.Sent), err)
	}
	page, err := e.svc.ListDispatches(t.Context(), actor(t, "company_admin"), companyA, outreach.DispatchQuery{})
	if err != nil || len(page.Items) != 1 || !eqInt(page.Items[0].Counts.Eligible, 100) {
		t.Fatalf("list = %+v, %v", page, err)
	}
	// Another org's dispatch is not found under this org.
	_, err = e.svc.GetDispatch(t.Context(), actor(t, "other_company_member"), companyB, d.ID)
	wantErr(t, err, outreach.ErrNotFound)
}

// TestRunDispatch_WaitsForPublication: a dispatch of a draft (or a post in
// review) waits — back to scheduled, nothing sent; post.published starts
// it; a post hidden or removed before the run ends it without sending and
// gives the unit back.
func TestRunDispatch_WaitsForPublication(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 2)
	ids := e.audience.add(10, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.consents.grant(consent.PurposeCommercialPush, ids...)
	p := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	e.posts.setStatus(p.ID, outreach.PostPendingReview)
	p.Status = outreach.PostPendingReview
	d := e.schedule(t, "company_admin", p, outreach.ChannelPush)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	if got := e.dispatch(t, d); got.Status != outreach.DispatchScheduled || e.sender.attempts != 0 {
		t.Fatalf("before publication: %s, %d sends", got.Status, e.sender.attempts)
	}
	// Published: post.published runs it.
	e.posts.setStatus(p.ID, outreach.PostPublished)
	pub, err := domainevent.New(domainevent.PostPublished{PostID: p.ID, OrgID: companyA, Kind: "text", Commercial: true},
		"post", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range e.subs.For(domainevent.TopicPostPublished) {
		if err := s.Handle(t.Context(), pub); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.dispatch(t, d); got.Status != outreach.DispatchCompleted || len(e.sender.to(notify.ChannelPush)) != 10 {
		t.Fatalf("after publication: %s, %d pushed", got.Status, len(e.sender.to(notify.ChannelPush)))
	}

	// Removed before the run: cancelled, unit back.
	q := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	d2 := e.schedule(t, "company_admin", q, outreach.ChannelPush)
	e.posts.setStatus(q.ID, outreach.PostRemoved)
	before := e.units(t, companyA)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	got := e.dispatch(t, d2)
	if got.Status != outreach.DispatchCancelled || got.LastError == nil || e.units(t, companyA) != before+1 {
		t.Fatalf("removed post: %s (%v), units %d → %d", got.Status, got.LastError, before, e.units(t, companyA))
	}
	if dto := outreach.ToDispatchDTO(outreach.DispatchView{Dispatch: got}); dto.FailureReason != "outreach.post_not_published" {
		t.Fatalf("failure reason %q", dto.FailureReason)
	}
}

// TestRunDispatch_AudienceTooSmall_FailsWithoutCount: an audience that
// fell below 10 since the post was compiled is never notified; the
// dispatch fails with outreach.audience_too_small (no number) and the
// company's unit goes back.
func TestRunDispatch_AudienceTooSmall_FailsWithoutCount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 1)
	ids := e.audience.add(9, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.consents.grant(consent.PurposeCommercialPush, ids...)
	d := e.schedule(t, "company_admin", e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), outreach.ChannelPush)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	got := e.dispatch(t, d)
	if got.Status != outreach.DispatchFailed || got.LastError == nil || *got.LastError != "outreach.audience_too_small" ||
		got.EligibleCount != nil || e.sender.attempts != 0 || e.units(t, companyA) != 1 {
		t.Fatalf("dispatch %s (%v), counts %v, sends %d, units %d", got.Status, got.LastError, deref(got.EligibleCount),
			e.sender.attempts, e.units(t, companyA))
	}
}

// TestScheduleDispatch_Rules: what a dispatch refuses before any unit is
// spent.
func TestScheduleDispatch_Rules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	cma := actor(t, "company_admin")
	sched := func(a authz.Actor, p outreach.PostInfo, commercial *bool, chans ...outreach.Channel) error {
		_, err := e.svc.ScheduleDispatch(ctx, outreach.DispatchInput{Actor: a, Post: p, Channels: chans, Commercial: commercial})
		return err
	}
	push := outreach.ChannelPush

	// No targeted_dispatch unit: 403 entitlements.not_entitled.
	company := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	if status, code := problemOf(sched(cma, company, nil, push)); status != 403 || code != "entitlements.not_entitled" {
		t.Fatalf("without a unit: %d %s", status, code)
	}
	e.grantDispatches(t, companyA, 5)
	for name, tc := range map[string]struct {
		a          authz.Actor
		post       func() outreach.PostInfo
		commercial *bool
		chans      []outreach.Channel
		status     int
		code       string
	}{
		"not targeted": {cma, func() outreach.PostInfo {
			p := e.targetedPost(companyA, outreach.AuthorCompany, "all")
			p.Targeted = false
			return p
		}, nil, []outreach.Channel{push}, 422, "outreach.post_not_targeted"},
		"hidden post": {cma, func() outreach.PostInfo {
			p := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
			p.Status = outreach.PostHidden
			return p
		}, nil, []outreach.Channel{push}, 409, "outreach.invalid_state"},
		"commercial in-app": {cma, func() outreach.PostInfo { return e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)) },
			nil, []outreach.Channel{outreach.ChannelInApp}, 422, "validation_failed"},
		"company not commercial": {cma, func() outreach.PostInfo {
			return e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
		}, ptr(false), []outreach.Channel{push}, 422, "validation_failed"},
		"no channel": {cma, func() outreach.PostInfo { return e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)) },
			nil, nil, 422, "validation_failed"},
		"channel twice": {cma, func() outreach.PostInfo { return e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)) },
			nil, []outreach.Channel{push, push}, 422, "validation_failed"},
		"unknown channel": {cma, func() outreach.PostInfo { return e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)) },
			nil, []outreach.Channel{"sms"}, 422, "validation_failed"},
		"other org's post": {actor(t, "other_company_member"), func() outreach.PostInfo {
			return e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
		}, nil, []outreach.Channel{push}, 404, "not_found"},
		"university names a club": {actor(t, "university_rep"), func() outreach.PostInfo {
			return e.targetedPost(uniOrgA, outreach.AuthorUniversity, uniKey(uniA), clubKey(clubA))
		}, nil, []outreach.Channel{push}, 422, "outreach.target_out_of_scope"},
		"club member": {actor(t, "club_member"), func() outreach.PostInfo {
			return e.targetedPost(clubA, outreach.AuthorClub, clubKey(clubA))
		}, nil, []outreach.Channel{push}, 403, "forbidden"},
	} {
		if status, code := problemOf(sched(tc.a, tc.post(), tc.commercial, tc.chans...)); status != tc.status || code != tc.code {
			t.Fatalf("%s: %d %s, want %d %s", name, status, code, tc.status, tc.code)
		}
	}
	if e.units(t, companyA) != 5 {
		t.Fatalf("a refused dispatch spent a unit: %d left", e.units(t, companyA))
	}

	// Commercial e-mail needs the unsubscribe endpoint; informational e-mail does not.
	noUnsub := newEnv(t, func(_ *env, d *outreach.Deps) { d.UnsubscribeURL = "" })
	noUnsub.grantDispatches(t, companyA, 1)
	_, err := noUnsub.svc.ScheduleDispatch(ctx, outreach.DispatchInput{Actor: cma,
		Post: noUnsub.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), Channels: []outreach.Channel{outreach.ChannelEmail}})
	if status, code := problemOf(err); status != 422 || code != "validation_failed" {
		t.Fatalf("commercial e-mail without unsubscribe: %d %s", status, code)
	}
	if _, err := noUnsub.svc.ScheduleDispatch(ctx, outreach.DispatchInput{Actor: actor(t, "university_rep"),
		Post: noUnsub.targetedPost(uniOrgA, outreach.AuthorUniversity, uniKey(uniA)), Channels: []outreach.Channel{outreach.ChannelEmail}}); err != nil {
		t.Fatalf("informational e-mail: %v", err)
	}

	// A sponsored club post is commercial; an informational post may opt into the filter.
	sponsored := e.targetedPost(clubA, outreach.AuthorClub, clubKey(clubA))
	sponsored.Commercial = true
	if status, code := problemOf(sched(actor(t, "club_admin"), sponsored, ptr(false), push)); status != 422 || code != "validation_failed" {
		t.Fatalf("sponsored post, commercial=false: %d %s", status, code)
	}
	uniPost := e.targetedPost(uniOrgA, outreach.AuthorUniversity, uniKey(uniA))
	id, err := e.svc.ScheduleDispatch(ctx, outreach.DispatchInput{Actor: actor(t, "university_rep"), Post: uniPost,
		Channels: []outreach.Channel{push}, Commercial: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := e.store.GetDispatch(ctx, uniOrgA, id); !d.Commercial || d.EntitlementUsageID != nil {
		t.Fatalf("university commercial dispatch %+v: consent filter, no unit", d)
	}

	// A suspended org dispatches nothing.
	e.orgs.set(companyB, orgs.KindCompany, orgs.StatusSuspended)
	_, err = e.svc.ScheduleDispatch(ctx, outreach.DispatchInput{Actor: actor(t, "other_company_member"),
		Post: e.targetedPost(companyB, outreach.AuthorCompany, uniKey(uniA)), Channels: []outreach.Channel{push}})
	wantErr(t, err, orgs.ErrOrgSuspended)
}

// TestDiscardDispatch_ReleasesAndDeletes: a post withdrawn before it was
// published takes its dispatch along (unit back, row gone); no dispatch is
// a no-op; a dispatch that ran is kept (409).
func TestDiscardDispatch_ReleasesAndDeletes(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	e.grantDispatches(t, companyA, 2)
	cma := actor(t, "company_admin")
	p := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	d := e.schedule(t, "company_admin", p, outreach.ChannelPush)
	if v, ok, err := e.svc.DispatchOfPost(ctx, companyA, p.ID); err != nil || !ok || v.ID != d.ID {
		t.Fatalf("DispatchOfPost = %s %v %v", v.ID, ok, err)
	}
	if err := e.svc.DiscardDispatch(ctx, cma, companyA, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.svc.DispatchOfPost(ctx, companyA, p.ID); ok || e.units(t, companyA) != 2 {
		t.Fatalf("after discard: still there %v, units %d", ok, e.units(t, companyA))
	}
	if err := e.svc.DiscardDispatch(ctx, cma, companyA, p.ID); err != nil {
		t.Fatalf("second discard: %v", err)
	}
	// A moderator's discard is not filed under the org (BE-032: the moderator stays out of its trail).
	q := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	e.schedule(t, "company_admin", q, outreach.ChannelPush)
	if err := e.svc.DiscardDispatch(ctx, actor(t, "moderator"), companyA, q.ID); err != nil {
		t.Fatal(err)
	}
	last := e.audit.last()
	if last.Action != string(authz.OutreachDispatchCancel) || last.OrgID != nil || last.Metadata["org_id"] != companyA.String() {
		t.Fatalf("moderator's discard audit = %+v", last)
	}
	// A dispatch that ran is kept.
	ids := e.audience.add(10, true, uniKey(uniA))
	e.users.add(false, ids...)
	r := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	e.schedule(t, "company_admin", r, outreach.ChannelPush)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	wantErr(t, e.svc.DiscardDispatch(ctx, cma, companyA, r.ID), outreach.ErrInvalidState)
}

// TestResumeDispatches_LostEventAndStaleClaim: outreach.resume_dispatches
// runs a scheduled dispatch whose event was lost (published post) and one
// left running by a crashed worker once its claim is older than the lease;
// a fresh claim is left alone.
func TestResumeDispatches_LostEventAndStaleClaim(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ids := e.audience.add(10, true, uniKey(uniA))
	e.users.add(false, ids...)
	mem, ok := e.store.(*outreach.MemStore)
	if !ok {
		t.Fatal("not the in-memory store")
	}
	lost := e.schedule(t, "university_rep", e.targetedPost(uniOrgA, outreach.AuthorUniversity, uniKey(uniA)), outreach.ChannelPush)
	crashed := e.schedule(t, "university_rep", e.targetedPost(uniOrgA, outreach.AuthorUniversity, uniKey(uniA), yearKey(3)),
		outreach.ChannelInApp)
	crashed.Status = outreach.DispatchRunning
	if err := mem.PutDispatch(crashed); err != nil {
		t.Fatal(err)
	}
	e.audience.add(10, true, uniKey(uniA), yearKey(3))
	job := func() {
		t.Helper()
		for _, j := range e.mod.Jobs() {
			if j.Name == outreach.ResumeDispatchesJobName {
				if err := j.Handle(t.Context()); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatal("no resume job")
	}
	job() // too early: the event may still come, the claim is fresh
	if e.dispatch(t, lost).Status != outreach.DispatchScheduled || e.dispatch(t, crashed).Status != outreach.DispatchRunning {
		t.Fatal("the resume job ran a fresh dispatch")
	}
	e.clk.Advance(20 * time.Minute)
	job()
	if e.dispatch(t, lost).Status != outreach.DispatchCompleted || e.dispatch(t, crashed).Status != outreach.DispatchCompleted {
		t.Fatalf("after the lease: %s, %s", e.dispatch(t, lost).Status, e.dispatch(t, crashed).Status)
	}
}

// TestDispatchSweeper_FinishedAfterTwoYears (02 §24.2 outreach_dispatch):
// a dry run only counts; an approved run deletes the finished dispatches
// older than the period and keeps an open one.
func TestDispatchSweeper_FinishedAfterTwoYears(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	mem, ok := e.store.(*outreach.MemStore)
	if !ok {
		t.Fatal("not the in-memory store")
	}
	old := t0.AddDate(-3, 0, 0)
	put := func(st outreach.DispatchStatus, at time.Time) uuid.UUID {
		d := outreach.Dispatch{ID: uuid.NewV7(), OrgID: companyA, PostID: uuid.NewV7(), AudienceKeys: []string{uniKey(uniA)},
			AudienceDimCount: 1, Channels: []string{"push"}, DeepLink: "/posts/x", Commercial: true, Status: st,
			CreatedBy: uuid.NewV7(), CreatedAt: at, UpdatedAt: at}
		if st.Terminal() {
			d.CompletedAt = &at
		}
		if err := mem.PutDispatch(d); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	gone1, gone2 := put(outreach.DispatchCompleted, old), put(outreach.DispatchCancelled, old)
	open := put(outreach.DispatchScheduled, old)
	young := put(outreach.DispatchCompleted, t0.AddDate(0, -1, 0))
	if codes := e.mod.Sweeper.PolicyCodes(); !slices.Equal(codes, []string{"outreach_dispatch"}) {
		t.Fatalf("policy codes %v", codes)
	}
	pol := privacy.RetentionPolicy{Code: "outreach_dispatch", Retention: 2 * 365 * 24 * time.Hour}
	res, err := e.mod.Sweeper.Sweep(t.Context(), pol, t0)
	if err != nil || res.Count != 2 || res.Table != "outreach_dispatches" {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if _, err := mem.DispatchByID(t.Context(), gone1); err != nil {
		t.Fatal("a dry run deleted")
	}
	pol.LegalApproved = true
	if res, err = e.mod.Sweeper.Sweep(t.Context(), pol, t0); err != nil || res.Count != 2 {
		t.Fatalf("approved run = %+v, %v", res, err)
	}
	for id, want := range map[uuid.UUID]bool{gone1: false, gone2: false, open: true, young: true} {
		if _, err := mem.DispatchByID(t.Context(), id); (err == nil) != want {
			t.Fatalf("dispatch %s kept = %v, want %v", id, err == nil, want)
		}
	}
	if _, err := e.mod.Sweeper.Sweep(t.Context(), privacy.RetentionPolicy{Code: "segment"}, t0); err == nil {
		t.Fatal("an unknown policy was swept")
	}
}

// TestDispatch_NoPersonalDataNoExactCounts: the dispatch's audit records
// and notifications hold IDs and fixed words only — no count, no name, no
// address (in-app and push data are opaque IDs; e-mail is the one place
// with the post's title and the org's name, never the recipient's).
func TestDispatch_NoPersonalDataNoExactCounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 1)
	ids := e.audience.add(13, true, uniKey(uniA))
	e.users.add(false, ids...)
	e.consents.grant(consent.PurposeCommercialPush, ids...)
	e.consents.grant(consent.PurposeCommercialEmail, ids...)
	e.schedule(t, "company_admin", e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA)), outreach.ChannelPush,
		outreach.ChannelEmail)
	if err := e.deliver(t); err != nil {
		t.Fatal(err)
	}
	for _, ev := range e.audit.all() {
		for k, v := range ev.Metadata {
			if strings.Contains(k, "count") {
				t.Fatalf("audit %s carries %s=%v", ev.Action, k, v)
			}
			if n, ok := v.(int); ok && n == 13 {
				t.Fatalf("audit %s carries the exact count in %s", ev.Action, k)
			}
		}
	}
	for _, m := range e.sender.messages() {
		for k, v := range m.Data {
			if s, ok := v.(string); ok && (strings.Contains(s, "Ada") || strings.Contains(s, "Yılmaz")) {
				t.Fatalf("message data %s holds the recipient's name", k)
			}
		}
	}
}

// ---- helpers

func sorted(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, uuid.UUID.Compare)
	return out
}

func eqInt(p *int, n int) bool { return p != nil && *p == n }

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestDispatchHTTP_CreateGetListCancel: the 4 operations over HTTP (04
// §2.8): createDispatch needs an Idempotency-Key (422 without), answers 201
// with null counts before the run; getDispatch and listDispatches show the
// counts as null or bucket; cancelDispatch answers the cancelled dispatch;
// an unknown post is 404.
func TestDispatchHTTP_CreateGetListCancel(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.grantDispatches(t, companyA, 1)
	app := newHTTP(t, e)
	p := e.targetedPost(companyA, outreach.AuthorCompany, uniKey(uniA))
	path := "/api/v1/posts/" + p.ID.String() + "/dispatches"
	body := map[string]any{"channels": []string{"push"}}

	if rec, _ := call(t, app, "company_admin", "POST", path, body, nil); rec.Code != 422 {
		t.Fatalf("without Idempotency-Key: %d", rec.Code)
	}
	rec, out := call(t, app, "company_admin", "POST", path, body, map[string]string{"Idempotency-Key": "dispatch-http-1"})
	if rec.Code != 201 || out["status"] != "scheduled" || out["commercial"] != true {
		t.Fatalf("create = %d %v", rec.Code, out)
	}
	for _, k := range []string{"eligible_count", "consented_count", "sent_count"} {
		if v, ok := out[k]; !ok || v != nil {
			t.Fatalf("%s = %v (present: %v), want null", k, v, ok)
		}
	}
	id, _ := out["dispatch_id"].(string)
	if rec, again := call(t, app, "company_admin", "POST", path, body, map[string]string{"Idempotency-Key": "dispatch-http-1"}); rec.Code != 201 ||
		again["dispatch_id"] != id || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay = %d %v", rec.Code, again)
	}
	orgPath := "/api/v1/orgs/" + companyA.String() + "/dispatches"
	if rec, got := call(t, app, "company_member", "GET", orgPath+"/"+id, nil, nil); rec.Code != 200 || got["dispatch_id"] != id {
		t.Fatalf("get = %d %v", rec.Code, got)
	}
	if rec, got := call(t, app, "platform_admin", "GET", orgPath, nil, nil); rec.Code != 200 || len(got["items"].([]any)) != 1 {
		t.Fatalf("list = %d %v", rec.Code, got)
	}
	if rec, got := call(t, app, "company_admin", "POST", orgPath+"/"+id+"/cancel", nil, nil); rec.Code != 200 || got["status"] != "cancelled" {
		t.Fatalf("cancel = %d %v", rec.Code, got)
	}
	if rec, _ := call(t, app, "company_admin", "POST", "/api/v1/posts/"+uuid.NewV7().String()+"/dispatches", body,
		map[string]string{"Idempotency-Key": "dispatch-http-2"}); rec.Code != 404 {
		t.Fatalf("unknown post = %d", rec.Code)
	}
	if rec, _ := call(t, app, "company_admin", "POST", path, map[string]any{"channels": []string{"sms"}},
		map[string]string{"Idempotency-Key": "dispatch-http-3"}); rec.Code != 422 {
		t.Fatalf("unknown channel = %d", rec.Code)
	}
	if rec, _ := call(t, app, "other_company_member", "GET", orgPath+"/"+id, nil, nil); rec.Code != 404 {
		t.Fatalf("another org's member = %d", rec.Code)
	}
}
