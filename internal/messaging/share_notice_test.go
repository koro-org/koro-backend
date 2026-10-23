package messaging_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/consent"
	"koro/internal/messaging"
	"koro/internal/notify"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// contactProof is a contact_info_share text the student was shown.
func contactProof() *students.ConsentProof {
	sum := sha256.Sum256([]byte("İletişim bilgisi paylaşım rızası"))
	return &students.ConsentProof{NoticeVersionID: uuid.NewV7(), TextSHA256: sum[:]}
}

// acceptSharing is an acceptance with share_contact and the shown text.
func acceptSharing(p *students.ConsentProof, ch consent.Channel) messaging.RespondInput {
	return messaging.RespondInput{Decision: messaging.DecisionAccepted, ShareContact: true, ContactConsent: p, Channel: ch}
}

// TestAccept_ShareContact_SameTx (T-335/T-345, 01 §6.3 "İletişim talebi
// kabulü"; BE-089): share_contact shares the e-mail with the request's
// company (students.ContactSharing with that company, the shown text, the
// consent channel and the request) in the transaction of the acceptance,
// its level 3 grant, its event and its audit record (share_contact: true). The same answer again
// changes nothing, the sharing included.
func TestAccept_ShareContact_SameTx(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	stu := actor(t, "student")
	v := e.send(t, "student")
	p := contactProof()
	got, err := e.svc.Respond(ctx, stu, v.Request.ID, acceptSharing(p, consent.ChannelMobile))
	if err != nil || got.Status != messaging.StatusAccepted {
		t.Fatalf("accept with share_contact = %+v, %v", got, err)
	}
	sh, gs := e.students.shared(), e.students.granted()
	if len(sh) != 1 || len(gs) != 1 {
		t.Fatalf("shares %+v, grants %+v", sh, gs)
	}
	if sh[0].student != stu.UserID || sh[0].company != e.companyA || sh[0].request != v.Request.ID || sh[0].channel != consent.ChannelMobile ||
		sh[0].proof.NoticeVersionID != p.NoticeVersionID || string(sh[0].proof.TextSHA256) != string(p.TextSHA256) {
		t.Fatalf("share = %+v", sh[0])
	}
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	rec := e.audit.of(authz.MessagingContactRequestRespond)
	if sh[0].tx != gs[0].tx || len(closed) != 1 || closed[0].tx != sh[0].tx || len(rec) != 1 || rec[0].tx != sh[0].tx {
		t.Fatalf("the sharing (tx %d) is not in the acceptance's transaction: grant %d, events %+v, audit %+v",
			sh[0].tx, gs[0].tx, closed, rec)
	}
	if rec[0].ev.Metadata["share_contact"] != true {
		t.Fatalf("respond audit = %+v", rec[0].ev.Metadata)
	}
	if _, err := e.svc.Respond(ctx, stu, v.Request.ID, acceptSharing(contactProof(), consent.ChannelWeb)); err != nil {
		t.Fatalf("accept again: %v", err)
	}
	if len(e.students.shared()) != 1 || len(e.students.granted()) != 1 {
		t.Fatal("the same answer again shared or granted again")
	}

	// Without share_contact the student's setting is not touched; a proof
	// sent without share_contact is ignored.
	w := e.send(t, "student_granted")
	plain := accept
	plain.ContactConsent = contactProof()
	if _, err := e.svc.Respond(ctx, actor(t, "student_granted"), w.Request.ID, plain); err != nil {
		t.Fatal(err)
	}
	if len(e.students.shared()) != 1 {
		t.Fatal("an acceptance without share_contact shared the contact details")
	}
	if rec := e.audit.of(authz.MessagingContactRequestRespond); rec[len(rec)-1].ev.Metadata["share_contact"] != false {
		t.Fatalf("respond audit = %+v", rec[len(rec)-1].ev.Metadata)
	}
}

// TestAccept_ShareContactFails_RollsBackAcceptance (T-335; BE-089): a
// sharing that cannot be written (here the consent module refuses the
// text) fails the request and rolls the whole acceptance back: the request
// stays pending, no grant, no event, no audit record. A retry with a good
// text accepts.
func TestAccept_ShareContactFails_RollsBackAcceptance(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	stu := actor(t, "student")
	v := e.send(t, "student")
	e.students.mu.Lock()
	e.students.failShare = consent.ErrTextMismatch
	e.students.mu.Unlock()
	_, err := e.svc.Respond(ctx, stu, v.Request.ID, acceptSharing(contactProof(), consent.ChannelWeb))
	wantErr(t, err, consent.ErrTextMismatch)
	got, _ := e.svc.GetMine(ctx, stu, v.Request.ID)
	if got.Status != messaging.StatusPending || got.Request.RespondedAt != nil || len(e.students.granted()) != 0 ||
		len(e.students.shared()) != 0 || len(e.outbox.of(domainevent.TopicContactRequestClosed)) != 0 ||
		len(e.audit.of(authz.MessagingContactRequestRespond)) != 0 {
		t.Fatalf("a failed sharing left the acceptance: %+v", got)
	}
	e.students.mu.Lock()
	e.students.failShare = nil
	e.students.mu.Unlock()
	if got, err = e.svc.Respond(ctx, stu, v.Request.ID, acceptSharing(contactProof(), consent.ChannelWeb)); err != nil ||
		got.Status != messaging.StatusAccepted || len(e.students.shared()) != 1 {
		t.Fatalf("retry = %+v, %v", got, err)
	}
}

// TestRespond_ShareContact_Rules (04 §2.9; BE-089): share_contact goes
// with an acceptance only (422 body.share_contact on a decline) and with
// the contact_info_share text shown (422 consent.proof_required); both are
// refused before anything is written. Under an Idempotency-Key another
// sharing choice is another request (422 idempotency.key_reused).
func TestRespond_ShareContact_Rules(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	stu := actor(t, "student")
	v := e.send(t, "student")
	declineSharing := acceptSharing(contactProof(), consent.ChannelWeb)
	declineSharing.Decision = messaging.DecisionDeclined
	_, err := e.svc.Respond(ctx, stu, v.Request.ID, declineSharing)
	wantErr(t, err, problem.ErrValidation)
	var pe *problem.Error
	if !errors.As(err, &pe) || len(pe.Fields()) != 1 || pe.Fields()[0].Location != "body.share_contact" {
		t.Fatalf("decline with share_contact = %v", err)
	}
	_, err = e.svc.Respond(ctx, stu, v.Request.ID, acceptSharing(nil, consent.ChannelWeb))
	wantCode(t, err, 422, "consent.proof_required")
	if got, _ := e.svc.GetMine(ctx, stu, v.Request.ID); got.Status != messaging.StatusPending ||
		len(e.students.granted()) != 0 || len(e.students.shared()) != 0 {
		t.Fatalf("a refused answer wrote: %+v", got)
	}

	kctx := idempotency.WithKey(ctx, idempotency.Key{Scope: stu.UserID.String() + ":POST /me/contact-requests/{request_id}/respond",
		Value: "respond-key-0001", Status: 200})
	p := contactProof()
	if _, err := e.svc.Respond(kctx, stu, v.Request.ID, acceptSharing(p, consent.ChannelWeb)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Respond(kctx, stu, v.Request.ID, acceptSharing(p, consent.ChannelWeb)); err != nil {
		t.Fatalf("replay: %v", err)
	}
	_, err = e.svc.Respond(kctx, stu, v.Request.ID, accept)
	wantErr(t, err, idempotency.ErrKeyReused)
	if len(e.students.shared()) != 1 {
		t.Fatalf("shares = %+v", e.students.shared())
	}
}

// TestNotification_PushAndEmail (T-336; BE-089): every notice goes in-app,
// by push and by e-mail. The student's in-app notice names the company as
// its source; the push carries the fixed text's data (the request's ID)
// and the deep link; the e-mail goes to identity's address in the user's
// language with the company's name and the link to the request, never the
// subject or the message. The company's e-mails carry the link only. A
// recipient without an address (erased) gets in-app and push. Each channel
// has its own dedupe key, so a redelivered task sends nothing twice.
func TestNotification_PushAndEmail(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	in := request(e.companyA, "student")
	in.Subject, in.Message = "Gizli konu satırı", "Bu mesaj gövdesi e-postaya asla girmemeli."
	v, err := e.svc.Create(ctx, actor(t, "company_member"), e.companyA, in)
	if err != nil {
		t.Fatal(err)
	}
	created := e.outbox.of(domainevent.TopicContactRequestCreated)
	for range 2 { // a redelivery
		if err := e.mod.HandleCreated(ctx, created[0].env); err != nil {
			t.Fatal(err)
		}
	}
	stu, id := authztest.ID("student"), v.Request.ID.String()
	link := "/me/contact-requests/" + id
	push, mail := e.sender.on(notify.ChannelPush), e.sender.on(notify.ChannelEmail)
	if len(e.sender.all()) != 3 || len(push) != 1 || len(mail) != 1 {
		t.Fatalf("notices = %+v", e.sender.all())
	}
	if p := push[0]; *p.UserID != stu || p.Template != "messaging.contact_request_received" || p.LinkPath != link ||
		p.SourceOrgID != nil || p.Category != notify.CategoryTransactional || !strings.HasSuffix(p.DedupeKey, ":push") ||
		len(p.Data) != 1 || p.Data["contact_request_id"] != id {
		t.Fatalf("push = %+v", p)
	}
	m := mail[0]
	if *m.UserID != stu || m.ToEmail != stu.String()+"@example.test" || m.Locale != "en" || m.LinkPath != "" ||
		m.Category != notify.CategoryTransactional || !strings.HasSuffix(m.DedupeKey, ":email") ||
		m.Data["company_name"] != "Şirket A" || m.Data["url"] != linkBase+link {
		t.Fatalf("e-mail = %+v", m)
	}
	b, _ := json.Marshal(e.sender.all())
	if strings.Contains(string(b), in.Subject) || strings.Contains(string(b), in.Message) {
		t.Fatalf("a notice carries the company's free text: %s", b)
	}

	// The company's notices: the link only (no student, no reason).
	before := len(e.sender.all())
	e.users.erase(authztest.ID("company_admin"))
	if _, err := e.svc.Respond(ctx, actor(t, "student"), v.Request.ID, decline); err != nil {
		t.Fatal(err)
	}
	e.orgs.leave(e.companyA, authztest.ID("company_member")) // → the owner and the admin
	closed := e.outbox.of(domainevent.TopicContactRequestClosed)
	if err := e.mod.HandleClosed(ctx, closed[0].env); err != nil {
		t.Fatal(err)
	}
	got := map[notify.Channel][]uuid.UUID{}
	for _, m := range e.sender.all()[before:] {
		got[m.Channel] = append(got[m.Channel], *m.UserID)
		if m.Template != "messaging.contact_request_declined" {
			t.Fatalf("notice = %+v", m)
		}
		if m.Channel == notify.ChannelEmail && (len(m.Data) != 1 ||
			m.Data["url"] != linkBase+"/orgs/"+e.companyA.String()+"/contact-requests/"+id) {
			t.Fatalf("company e-mail data = %+v", m.Data)
		}
	}
	owner, admin := authztest.ID("company_owner_a"), authztest.ID("company_admin")
	if len(got[notify.ChannelInApp]) != 2 || len(got[notify.ChannelPush]) != 2 ||
		!slices.Contains(got[notify.ChannelPush], admin) || !slices.Equal(got[notify.ChannelEmail], []uuid.UUID{owner}) {
		t.Fatalf("company notices by channel = %v (the erased admin gets no e-mail)", got)
	}
}

// TestNotification_PreferenceOff_ChannelSkipped (T-336, 02 §6.5; BE-089):
// over the real notify service, a student who switched the
// contact_requests e-mail off gets the request in-app and by push but no
// e-mail (skipped_preference); the company, whose preferences are at their
// default, gets all three, the e-mail rendered in its language with the
// transactional footer and the link.
func TestNotification_PreferenceOff_ChannelSkipped(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	mt, push := notify.NewMemTransport(), &pushRecorder{}
	var nm notify.Module
	e := newEnv(t, func(d *messaging.Deps) {
		nm = notify.New(notify.Deps{Store: notify.NewMemStore(), Tx: db.NopTx{}, Transport: mt, Templates: tpl, Clock: d.Clock,
			Log: obs.Discard(), FromAddress: "noreply@koro.test", Recipients: indexer{}, Audit: nopAudit{}, Push: push})
		d.Sender = nm.Service
	})
	ctx := t.Context()
	stu := actor(t, "student")
	if _, err := nm.Service.PutMyPreferences(ctx, stu, []notify.Preference{
		{Category: notify.PrefContactRequests, Channel: notify.ChannelEmail, Enabled: false},
	}); err != nil {
		t.Fatal(err)
	}
	v := e.send(t, "student")
	if err := e.mod.HandleCreated(ctx, e.outbox.of(domainevent.TopicContactRequestCreated)[0].env); err != nil {
		t.Fatal(err)
	}
	if n := len(mt.Sent()); n != 0 {
		t.Fatalf("e-mails = %d, want 0 (switched off)", n)
	}
	if p := push.of(stu.UserID); len(p) != 1 || p[0].Kind != "messaging.contact_request_received" ||
		p[0].LinkPath != "/me/contact-requests/"+v.Request.ID.String() || p[0].Title == "" {
		t.Fatalf("push = %+v", p)
	}
	inbox, err := nm.Service.ListMine(ctx, stu, notify.InAppQuery{})
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].Kind != "messaging.contact_request_received" ||
		inbox.Items[0].SourceOrgID == nil || *inbox.Items[0].SourceOrgID != e.companyA {
		t.Fatalf("in-app = %+v, %v", inbox, err)
	}

	if _, err := e.svc.Respond(ctx, stu, v.Request.ID, accept); err != nil {
		t.Fatal(err)
	}
	if err := e.mod.HandleClosed(ctx, e.outbox.of(domainevent.TopicContactRequestClosed)[0].env); err != nil {
		t.Fatal(err)
	}
	cmm := authztest.ID("company_member")
	sent := mt.Sent()
	if len(sent) != 1 || sent[0].To != cmm.String()+"@example.test" || sent[0].Subject != "Your contact request was accepted" ||
		!strings.Contains(sent[0].Text, linkBase+"/orgs/"+e.companyA.String()+"/contact-requests/"+v.Request.ID.String()) ||
		!strings.Contains(sent[0].Text, "it is not a commercial electronic message") {
		t.Fatalf("company e-mail = %+v", sent)
	}
	if strings.Contains(sent[0].Text, stu.UserID.String()) || strings.Contains(sent[0].HTML, stu.UserID.String()) {
		t.Fatal("the company's e-mail names the student")
	}
	if p := push.of(cmm); len(p) != 1 || p[0].Kind != "messaging.contact_request_accepted" {
		t.Fatalf("company push = %+v", p)
	}
}

// pushRecorder is a PushSender that records the pushes it delivers.
type pushRecorder struct {
	mu   sync.Mutex
	sent []notify.PushMessage
}

func (*pushRecorder) Name() string { return "test-push" }

func (p *pushRecorder) SendPush(_ context.Context, m notify.PushMessage) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, m)
	return "", nil
}

func (p *pushRecorder) of(user uuid.UUID) []notify.PushMessage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []notify.PushMessage
	for _, m := range p.sent {
		if m.UserID == user {
			out = append(out, m)
		}
	}
	return out
}

// indexer is notify's recipient blind index for the tests.
type indexer struct{}

func (indexer) Index(email string) []byte {
	sum := sha256.Sum256([]byte(email))
	return sum[:]
}

// nopAudit is notify's audit port (its own records are not under test).
type nopAudit struct{}

func (nopAudit) Log(context.Context, audit.Event) error { return nil }
