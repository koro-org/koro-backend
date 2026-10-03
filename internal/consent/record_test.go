package consent_test

import (
	"bytes"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/httpx"
)

// TestRecord_AppendOnly_WithdrawCreatesNewRow (02 §4.3): a withdrawal never
// touches the grant; it is a second row and the newest row is the state.
func TestRecord_AppendOnly_WithdrawCreatesNewRow(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := actor(t, "student").UserID
	text := e.consentText(t, consent.PurposeContactInfoShare, "2026.10.01")

	ctx = httpx.WithUserAgent(httpx.WithClientIP(ctx, "203.0.113.9"), "KoroTest/1.0")
	g, err := e.svc.Record(ctx, grant(user, text, consent.ChannelWeb))
	if err != nil {
		t.Fatal(err)
	}
	before, err := e.store.ConsentByID(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(time.Minute)
	w := e.record(t, withdraw(user, consent.PurposeContactInfoShare))

	rows, err := e.store.AllOf(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != g || rows[1].ID != w {
		t.Fatalf("ledger = %+v, want the grant and then the withdrawal", rows)
	}
	after := rows[0]
	if after.Action != consent.Granted || !bytes.Equal(after.TextSHA256, before.TextSHA256) ||
		*after.NoticeVersionID != text.ID || !after.OccurredAt.Equal(before.OccurredAt) {
		t.Fatalf("grant changed: %+v → %+v", before, after)
	}
	if after.IP != "203.0.113.9" || after.UserAgent != "KoroTest/1.0" || after.ScopeType != consent.ScopeNone {
		t.Fatalf("grant proof = ip %q ua %q scope %s", after.IP, after.UserAgent, after.ScopeType)
	}
	if rows[1].Action != consent.Withdrawn || rows[1].NoticeVersionID != nil || rows[1].TextSHA256 != nil {
		t.Fatalf("withdrawal = %+v", rows[1])
	}
	on, err := e.svc.HasActive(ctx, user, consent.PurposeContactInfoShare, nil)
	if err != nil || on {
		t.Fatalf("HasActive after withdrawal = %v, %v", on, err)
	}

	// Every act is audited as consent.record.create by the subject.
	evs := e.audit.of(string(authz.ConsentRecordCreate))
	if len(evs) != 2 || *evs[0].ActorUserID != user || evs[1].Metadata["consent_action"] != "withdrawn" {
		t.Fatalf("audit = %+v", evs)
	}
}

// TestHasActive_LatestActionWins: the newest row of (user, purpose, scope)
// decides; other users, purposes and scopes do not interfere; an inactive
// purpose is never in force.
func TestHasActive_LatestActionWins(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user, other := actor(t, "student").UserID, actor(t, "student_b").UserID
	text := e.consentText(t, consent.PurposeCommercialPush, "2026.10.01")
	sponsor := e.consentText(t, consent.PurposeEventSponsor, "2026.10.01")

	check := func(u uuid.UUID, p consent.Purpose, scope *uuid.UUID, want bool) {
		t.Helper()
		got, err := e.svc.HasActive(ctx, u, p, scope)
		if err != nil || got != want {
			t.Fatalf("HasActive(%s, %s, %v) = %v, %v; want %v", u, p, scope, got, err, want)
		}
	}
	check(user, consent.PurposeCommercialPush, nil, false) // never asked
	e.record(t, grant(user, text, consent.ChannelMobile))
	check(user, consent.PurposeCommercialPush, nil, true)
	e.record(t, withdraw(user, consent.PurposeCommercialPush))
	check(user, consent.PurposeCommercialPush, nil, false)
	e.record(t, grant(user, text, consent.ChannelWeb))
	check(user, consent.PurposeCommercialPush, nil, true)
	check(other, consent.PurposeCommercialPush, nil, false)
	check(user, consent.PurposeCommercialEmail, nil, false)

	// Same instant: the larger (later) UUIDv7 wins.
	e.record(t, withdraw(user, consent.PurposeCommercialPush))
	in := grant(user, text, consent.ChannelWeb)
	if _, err := e.svc.Record(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Record(ctx, withdraw(user, consent.PurposeCommercialPush)); err != nil {
		t.Fatal(err)
	}
	check(user, consent.PurposeCommercialPush, nil, false)

	// Per-registration purpose: each scope has its own state.
	regA, regB := uuid.NewV7(), uuid.NewV7()
	g := grant(user, sponsor, consent.ChannelWeb)
	g.ScopeID = &regA
	e.record(t, g)
	check(user, consent.PurposeEventSponsor, &regA, true)
	check(user, consent.PurposeEventSponsor, &regB, false)
	if _, err := e.svc.HasActive(ctx, user, consent.PurposeEventSponsor, nil); err == nil {
		t.Fatal("HasActive of a per-registration purpose without scope must fail")
	}

	// Switching the purpose off ends every grant at once; the ledger stays.
	e.record(t, grant(user, text, consent.ChannelWeb))
	if _, err := e.svc.UpdatePurpose(ctx, actor(t, "dpo"), consent.PurposeCommercialPush, consent.PurposePatch{Active: ptr(false)}, proof); err != nil {
		t.Fatal(err)
	}
	check(user, consent.PurposeCommercialPush, nil, false)
}

// TestFilterActive_Batch: one call answers for many users, keeps only the
// current grants, drops duplicates and unknown users.
func TestFilterActive_Batch(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	var users []uuid.UUID
	for range 25 {
		users = append(users, uuid.NewV7())
	}
	var want []uuid.UUID
	for i, u := range users {
		switch i % 3 {
		case 0: // granted
			e.record(t, grant(u, text, consent.ChannelWeb))
			want = append(want, u)
		case 1: // granted, then withdrawn
			e.record(t, grant(u, text, consent.ChannelWeb))
			e.record(t, withdraw(u, consent.PurposeCommercialEmail))
		case 2: // withdrawn, then granted again
			e.record(t, withdraw(u, consent.PurposeCommercialEmail))
			e.record(t, grant(u, text, consent.ChannelMobile))
			want = append(want, u)
		}
	}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	query := append(slices.Clone(users), users[0], uuid.NewV7()) // a duplicate and a stranger
	got, err := e.svc.FilterActive(ctx, query, consent.PurposeCommercialEmail)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("FilterActive = %d users, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	if got, err := e.svc.FilterActive(ctx, nil, consent.PurposeCommercialEmail); err != nil || len(got) != 0 {
		t.Fatalf("FilterActive(nil) = %v, %v", got, err)
	}
	if got, err := e.svc.FilterActive(ctx, users, consent.PurposeCommercialSMS); err != nil || len(got) != 0 {
		t.Fatalf("FilterActive of another purpose = %v, %v", got, err)
	}
	if _, err := e.svc.FilterActive(ctx, users, consent.PurposeEventSponsor); err == nil {
		t.Fatal("FilterActive of a per-registration purpose must fail")
	}
}

// TestRecord_IYSPurpose_EmitsOutboxEvent (ADR-0009): a commercial e-mail
// consent writes a pending İYS submission and consent.recorded in the same
// transaction; the subscription delivers it.
func TestRecord_IYSPurpose_EmitsOutboxEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := actor(t, "student").UserID
	text := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	id := e.record(t, grant(user, text, consent.ChannelWeb))

	evs := e.out.Events()
	if len(evs) != 1 || evs[0].Topic != domainevent.TopicConsentRecorded || evs[0].AggregateID != id {
		t.Fatalf("outbox = %+v", evs)
	}
	p, err := domainevent.Decode[domainevent.ConsentRecorded](evs[0])
	if err != nil || p.ConsentID != id || p.UserID != user || p.Purpose != "commercial_email" || p.Action != "granted" {
		t.Fatalf("payload = %+v, %v", p, err)
	}
	sub, err := e.store.SubmissionOf(ctx, id)
	if err != nil || sub.Status != consent.SubmissionPending || sub.Channel != consent.IYSEmail || sub.Attempts != 0 {
		t.Fatalf("submission = %+v, %v", sub, err)
	}

	for _, err := range e.deliverAll(t) {
		if err != nil {
			t.Fatal(err)
		}
	}
	sub, _ = e.store.SubmissionOf(ctx, id)
	sent := e.iys.Sent()
	if sub.Status != consent.SubmissionSent || sub.SentAt == nil || sub.IYSTransactionID == nil || sub.Attempts != 1 ||
		len(sent) != 1 || sent[0].ConsentID != id || sent[0].Action != consent.Granted || sent[0].Channel != consent.IYSEmail {
		t.Fatalf("after delivery: %+v, sent %+v", sub, sent)
	}
	// At least once: a second delivery of the same event is a no-op.
	for _, err := range e.deliverAll(t) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(e.iys.Sent()) != 1 {
		t.Fatalf("redelivery submitted again: %d", len(e.iys.Sent()))
	}

	// A withdrawal of a commercial purpose reaches İYS too.
	w := e.record(t, withdraw(user, consent.PurposeCommercialEmail))
	if got := e.out.Topics(); len(got) != 2 {
		t.Fatalf("topics after withdrawal = %v", got)
	}
	if sub, err := e.store.SubmissionOf(ctx, w); err != nil || sub.Status != consent.SubmissionPending {
		t.Fatalf("withdrawal submission = %+v, %v", sub, err)
	}
}

// TestRecord_NonIYSPurpose_NoEvent: purposes without İYS write neither a
// submission nor an outbox event.
func TestRecord_NonIYSPurpose_NoEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := actor(t, "student").UserID
	for _, p := range []consent.Purpose{consent.PurposeCommercialPush, consent.PurposeDiscoveryProfile, consent.PurposeContactInfoShare} {
		text := e.consentText(t, p, "2026.10.01")
		id := e.record(t, grant(user, text, consent.ChannelWeb))
		if _, err := e.store.SubmissionOf(ctx, id); err == nil {
			t.Fatalf("%s: unexpected İYS submission", p)
		}
		e.record(t, withdraw(user, p))
	}
	if got := e.out.Events(); len(got) != 0 {
		t.Fatalf("outbox = %v, want nothing", e.out.Topics())
	}
}

// TestRecord_Rules pins the consent rules of 03 k12, 02 §4.3 and Kurul
// 2026/347 / 2025/1072.
func TestRecord_Rules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	user := actor(t, "student").UserID
	email := e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	sponsor := e.consentText(t, consent.PurposeEventSponsor, "2026.10.01")
	privacyNotice := e.publish(t, consent.NoticeDraft{Kind: consent.KindPrivacyNotice, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.10.01", Title: "Aydınlatma", BodyMD: "Aydınlatma metni.\n"})
	terms := e.publish(t, consent.NoticeDraft{Kind: consent.KindTermsOfUse, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2026.10.01", Title: "Kullanım koşulları", BodyMD: "Koşullar.\n"})
	draftPurpose := consent.PurposeCommercialEmail
	draft, err := e.svc.CreateNotice(ctx, actor(t, "dpo"), consent.NoticeDraft{Kind: consent.KindConsentText, Purpose: &draftPurpose,
		Audience: consent.AudienceStudent, Locale: consent.LocaleTR, Version: "2026.11.01", Title: "Taslak", BodyMD: "Taslak.\n"}, proof)
	if err != nil {
		t.Fatal(err)
	}
	reg := uuid.NewV7()
	nilID := uuid.Nil()

	tests := []struct {
		name string
		in   func() consent.RecordInput
		want error
	}{
		{"minor grants commercial", func() consent.RecordInput { in := grant(user, email, consent.ChannelWeb); in.Minor = true; return in }, consent.ErrMinorRestricted},
		{"minor grants sponsor sharing", func() consent.RecordInput {
			in := grant(user, sponsor, consent.ChannelWeb)
			in.ScopeID, in.Minor = &reg, true
			return in
		}, consent.ErrMinorRestricted},
		{"inactive purpose", func() consent.RecordInput {
			in := grant(user, email, consent.ChannelWeb)
			in.Purpose = consent.PurposeActivityRanking
			return in
		}, consent.ErrPurposeInactive},
		{"unknown purpose", func() consent.RecordInput { in := withdraw(user, "selfie_share"); return in }, consent.ErrUnknownPurpose},
		{"grant via e-mail link", func() consent.RecordInput { return grant(user, email, consent.ChannelEmailLink) }, consent.ErrChannelNotAllowed},
		{"grant without text", func() consent.RecordInput {
			in := grant(user, email, consent.ChannelWeb)
			in.NoticeVersionID, in.TextSHA256 = nil, nil
			return in
		}, consent.ErrProofRequired},
		{"version without digest", func() consent.RecordInput {
			in := grant(user, email, consent.ChannelWeb)
			in.TextSHA256 = nil
			return in
		}, consent.ErrProofRequired},
		{"privacy notice as consent proof", func() consent.RecordInput {
			in := grant(user, email, consent.ChannelWeb)
			in.NoticeVersionID, in.TextSHA256 = &privacyNotice.ID, privacyNotice.BodySHA256
			return in
		}, consent.ErrNoticeMismatch},
		{"terms of use as consent proof", func() consent.RecordInput {
			in := grant(user, email, consent.ChannelWeb)
			in.NoticeVersionID, in.TextSHA256 = &terms.ID, terms.BodySHA256
			return in
		}, consent.ErrNoticeMismatch},
		{"text of another purpose", func() consent.RecordInput {
			in := grant(user, sponsor, consent.ChannelWeb)
			in.Purpose = consent.PurposeCommercialEmail
			return in
		}, consent.ErrNoticeMismatch},
		{"digest of another text", func() consent.RecordInput {
			in := grant(user, email, consent.ChannelWeb)
			in.TextSHA256 = sponsor.BodySHA256
			return in
		}, consent.ErrTextMismatch},
		{"draft text", func() consent.RecordInput {
			in := grant(user, email, consent.ChannelWeb)
			in.NoticeVersionID, in.TextSHA256 = &draft.ID, draft.BodySHA256
			return in
		}, consent.ErrNoticeNotCurrent},
		{"per-registration purpose without scope", func() consent.RecordInput { return grant(user, sponsor, consent.ChannelWeb) }, consent.ErrScopeRequired},
		{"nil scope", func() consent.RecordInput {
			in := grant(user, sponsor, consent.ChannelWeb)
			in.ScopeID = &nilID
			return in
		}, consent.ErrScopeRequired},
		{"scope on an unscoped purpose", func() consent.RecordInput { in := grant(user, email, consent.ChannelWeb); in.ScopeID = &reg; return in }, consent.ErrScopeNotAllowed},
	}
	for _, tt := range tests {
		_, err := e.svc.Record(ctx, tt.in())
		if err == nil {
			t.Fatalf("%s: recorded", tt.name)
		}
		wantErr(t, err, tt.want)
	}
	if rows, _ := e.store.AllOf(ctx, user); len(rows) != 0 {
		t.Fatalf("rejected records left %d rows", len(rows))
	}

	// Withdrawing is always allowed: minors, inactive purposes, e-mail links.
	for _, in := range []consent.RecordInput{
		{UserID: user, Purpose: consent.PurposeCommercialEmail, Action: consent.Withdrawn, Channel: consent.ChannelEmailLink, Minor: true},
		{UserID: user, Purpose: consent.PurposeActivityRanking, Action: consent.Withdrawn, Channel: consent.ChannelWeb},
		{UserID: user, Purpose: consent.PurposeEventSponsor, Action: consent.Withdrawn, Channel: consent.ChannelWeb, ScopeID: &reg},
	} {
		if _, err := e.svc.Record(ctx, in); err != nil {
			t.Fatalf("withdraw %s: %v", in.Purpose, err)
		}
	}
	// A record through the admin channel is audited as the platform's act
	// on the subject.
	e.record(t, consent.RecordInput{UserID: user, Purpose: consent.PurposeDiscoveryProfile, Action: consent.Withdrawn, Channel: consent.ChannelAdmin})
	evs := e.audit.of(string(authz.ConsentRecordCreate))
	last := evs[len(evs)-1]
	if last.ActorUserID != nil || last.SystemJob == "" || last.Metadata[audit.MetaSubjectUserID] != user.String() {
		t.Fatalf("admin-channel audit = %+v", last)
	}
}

// TestRecordMine_ActorRules: recordMyConsent records for the actor only,
// takes k12 from the actor and replays an Idempotency-Key.
func TestRecordMine_ActorRules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	text := e.consentText(t, consent.PurposeDiscoveryProfile, "2026.10.01")
	in := consent.MyConsent{Purpose: consent.PurposeDiscoveryProfile, Action: consent.Granted, NoticeVersionID: &text.ID,
		TextSHA256: text.BodySHA256, Channel: consent.ChannelMobile}

	_, err := e.svc.RecordMine(ctx, actor(t, "student_minor"), in)
	wantErr(t, err, consent.ErrMinorRestricted)
	_, err = e.svc.RecordMine(ctx, authz.Anonymous(), in)
	wantErr(t, err, authz.ErrForbidden)

	c, err := e.svc.RecordMine(ctx, actor(t, "student"), in)
	if err != nil || c.UserID != actor(t, "student").UserID || c.Channel != consent.ChannelMobile {
		t.Fatalf("RecordMine = %+v, %v", c, err)
	}
	mine, err := e.svc.ListMine(ctx, actor(t, "student"), consent.PageQuery{})
	if err != nil || len(mine.Current) != 1 || mine.Current[0].Action != consent.Granted || len(mine.Page.Items) != 1 {
		t.Fatalf("ListMine = %+v, %v", mine, err)
	}
	other, _ := e.svc.ListMine(ctx, actor(t, "student_b"), consent.PageQuery{})
	if len(other.Current) != 0 || len(other.Page.Items) != 0 {
		t.Fatalf("another user's ledger leaked: %+v", other)
	}
}
