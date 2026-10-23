package ledger_test

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/ledger"
	"koro/internal/notify"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/money"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// tmplEarly is the security notice of a transfer dated before its approval
// (00 T-298, BE-092).
const tmplEarly = "ledger.payout_paid_before_approval"

// dispatchAll runs every subscription of every recorded event once, as the
// worker would.
func (e *payoutEnv) dispatchAll(t *testing.T, reg *outbox.Registry) {
	t.Helper()
	for _, env := range e.outbox.Events() {
		for _, s := range reg.For(env.Topic) {
			if err := s.Handle(t.Context(), env); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		}
	}
}

// earlyNotices returns the security notices sent so far.
func (e *payoutEnv) earlyNotices() []notify.Message {
	return slices.DeleteFunc(e.sender.all(), func(m notify.Message) bool { return m.Template != tmplEarly })
}

// TestMarkPaid_PaidBeforeApproval_SecurityNotice (00 T-298, BE-092): a
// transfer dated before the approval is recorded (the ledger follows the
// bank) and flagged in the audit; the payout.paid row of the same
// transaction carries the security notice, which reaches every finance and
// platform admin but the one who marked it paid: in-app (fixed text, the
// payout link, IDs only) and by e-mail where the admin has an address
// (payout, amount, masked IBAN only, approver and marker). A repeated task
// produces the same dedupe keys (notify makes it a no-op).
func TestMarkPaid_PaidBeforeApproval_SecurityNotice(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	p, _, _ := e.approved(t) // created by finance_admin_2, approved by finance_admin at t0
	marker := actor(t, "finance_admin_2")
	early := t0.Add(-90 * time.Minute)
	receipt := e.files.put(files.PurposePayoutReceipt)
	v, err := e.svc.AdminMarkPayoutPaid(ctxOf(t, marker), marker, p.ID,
		ledger.MarkPaidInput{BankReference: "EFT-EARLY-1", ReceiptFileID: &receipt, PaidAt: &early})
	if err != nil {
		t.Fatalf("mark-paid dated before the approval: %v", err)
	}
	if v.Status != ledger.StatusPaid || v.PaidAt == nil || !v.PaidAt.Equal(early) || !v.PaidBeforeApproval() {
		t.Fatalf("paid payout %+v", v.Payout)
	}
	rows := e.audit.of(authz.LedgerPayoutMarkPaid)
	if len(rows) != 1 || rows[0].Metadata["paid_before_approval"] != true {
		t.Fatalf("mark-paid audit %+v", rows)
	}
	if got := e.outbox.Topics(); slices.Index(got, domainevent.TopicPayoutPaid) < 0 || len(got) != 3 {
		t.Fatalf("events %v: want payout.created, .approved, .paid (no extra topic)", got)
	}

	e.dispatchAll(t, reg)
	notices := e.earlyNotices()
	inApp := map[uuid.UUID]notify.Message{}
	mail := map[string]notify.Message{}
	for _, m := range notices {
		if m.Category != notify.CategoryTransactional {
			t.Errorf("notice %s is %s, want transactional", m.DedupeKey, m.Category)
		}
		switch m.Channel {
		case notify.ChannelInApp:
			inApp[*m.UserID] = m
		case notify.ChannelEmail:
			mail[m.ToEmail] = m
		default:
			t.Errorf("unexpected channel %s", m.Channel)
		}
	}
	wantInApp := []uuid.UUID{authztest.ID("finance_admin"), authztest.ID("platform_admin"), authztest.ID("platform_admin_2")}
	if len(inApp) != len(wantInApp) {
		t.Fatalf("in-app notices to %v, want %v", keys(inApp), wantInApp)
	}
	for _, id := range wantInApp {
		m, ok := inApp[id]
		switch {
		case !ok:
			t.Errorf("no in-app notice to %s", id)
		case m.LinkPath != "/admin/payouts/"+p.ID.String() || len(m.Data) != 1 || m.Data["payout_id"] != p.ID.String():
			t.Errorf("in-app notice %+v", m)
		}
	}
	if _, ok := inApp[marker.UserID]; ok {
		t.Error("the admin who marked the payout paid was notified")
	}
	// platform_admin_2 has no address: in-app only.
	if len(mail) != 2 {
		t.Fatalf("e-mails to %v, want finance_admin's and platform_admin's", keys(mail))
	}
	for addr, wantName := range map[string]string{"fin1@example.test": "Fin", "pad1@example.test": "Pad"} {
		m, ok := mail[addr]
		if !ok {
			t.Errorf("no e-mail to %s", addr)
			continue
		}
		d := m.Data
		if d["name"] != wantName || d["payout_id"] != p.ID.String() || d["amount"] != money.Decimal(p.Amount) ||
			d["currency"] != "TRY" || d["bank_reference"] != "EFT-EARLY-1" || d["approved_by"] != "Fin Bir" ||
			d["paid_by"] != "Fin İki" || d["iban_masked"] != p.MaskedIBAN() || d["holder_name"] != "Örnek Vakfı" ||
			d["paid_at"] == "" || d["approved_at"] == "" || d["paid_at"] == d["approved_at"] {
			t.Errorf("e-mail to %s: %+v", addr, d)
		}
		for k, val := range d {
			if s, _ := val.(string); strings.Contains(strings.ReplaceAll(s, " ", ""), p.IBANSnapshot) {
				t.Errorf("e-mail to %s carries the IBAN in clear (%s)", addr, k)
			}
		}
	}

	// A repeated task sends the same deliveries (same dedupe keys).
	keysOf := func(ms []notify.Message) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.DedupeKey)
		}
		slices.Sort(out)
		return out
	}
	first := keysOf(notices)
	if len(slices.Compact(slices.Clone(first))) != len(first) {
		t.Fatalf("dedupe keys collide: %v", first)
	}
	e.dispatchAll(t, reg)
	if again := keysOf(e.earlyNotices()); !slices.Equal(again, slices.Sorted(slices.Values(append(slices.Clone(first), first...)))) {
		t.Fatalf("repeated task dedupe keys %v, want twice %v", again, first)
	}
}

// TestMarkPaid_AfterApproval_NoSecurityNotice: a transfer dated at (or
// after) the approval is the normal flow: no audit flag and no security
// notice (the regular payout.paid notices still go).
func TestMarkPaid_AfterApproval_NoSecurityNotice(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	p, _, _ := e.approved(t)
	v := e.markPaid(t, "finance_admin_2", p.ID) // paid_at = now = the approval instant
	if v.ApprovedAt == nil || v.PaidAt == nil || !v.PaidAt.Equal(*v.ApprovedAt) || v.PaidBeforeApproval() {
		t.Fatalf("paid payout %+v", v.Payout)
	}
	if rows := e.audit.of(authz.LedgerPayoutMarkPaid); len(rows) != 1 || rows[0].Metadata["paid_before_approval"] != nil {
		t.Fatalf("mark-paid audit %+v", rows)
	}
	e.dispatchAll(t, reg)
	if n := e.earlyNotices(); len(n) != 0 {
		t.Fatalf("security notices for an on-time transfer: %+v", n)
	}
	if !slices.ContainsFunc(e.sender.all(), func(m notify.Message) bool { return m.Template == "ledger.payout_paid" }) {
		t.Fatal("the regular payout.paid notices were not sent")
	}
}

// TestPayout_PaidBeforeApprovalSubscription pins the security notice's
// subscription: a second subscriber of payout.paid (the outbox row of the
// mark-paid transaction), on the critical queue.
func TestPayout_PaidBeforeApprovalSubscription(t *testing.T) {
	t.Parallel()
	e := newPayoutEnv(t)
	reg := outbox.NewRegistry()
	e.mod.Subscribe(reg)
	i := slices.IndexFunc(reg.For(domainevent.TopicPayoutPaid), func(s outbox.Subscription) bool {
		return s.Name == ledger.NotifyPayoutPaidBeforeApprovalTaskName
	})
	if i < 0 {
		t.Fatalf("payout.paid has no %s subscription", ledger.NotifyPayoutPaidBeforeApprovalTaskName)
	}
	if q := reg.For(domainevent.TopicPayoutPaid)[i].Queue; q != queue.Critical {
		t.Fatalf("security notice on queue %q, want critical", q)
	}
}

// TestPayout_PaidBeforeApprovalTemplate_SecurityCategory: the notice's
// template is transactional in the security preference category, which no
// one can switch off (02 §6.5 CHECK, notify.Allowed), on e-mail and in-app;
// it renders in both locales with the masked IBAN and a fixed in-app text.
func TestPayout_PaidBeforeApprovalTemplate_SecurityCategory(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatal(err)
	}
	meta, ok := tpl.Meta(tmplEarly)
	if !ok || meta.Category != notify.CategoryTransactional || meta.Preference != notify.PrefSecurity ||
		!meta.Supports(notify.ChannelEmail) || !meta.Supports(notify.ChannelInApp) {
		t.Fatalf("meta %+v, %v: want transactional, security, e-mail and in-app", meta, ok)
	}
	data := map[string]any{"name": "Fin", "payout_id": uuid.NewV7().String(), "holder_name": "Örnek Vakfı",
		"amount": "10800.00", "currency": "TRY", "iban_masked": "TR33 **** **** **** **** 8413 26", "bank_reference": "EFT-1",
		"approved_at": "04.10.2026 12:00", "paid_at": "04.10.2026 10:30", "approved_by": "Fin Bir", "paid_by": "Fin İki"}
	for _, loc := range []string{"tr", "en"} {
		r, err := tpl.Render(tmplEarly, loc, notify.RenderInput{Data: data})
		if err != nil {
			t.Fatalf("Render(%s): %v", loc, err)
		}
		for _, want := range []string{"TR33 **** **** **** **** 8413 26", "10800.00", "Fin Bir", "Fin İki", "EFT-1"} {
			if !strings.Contains(r.Text, want) || !strings.Contains(r.HTML, want) {
				t.Errorf("Render(%s) misses %q:\n%s", loc, want, r.Text)
			}
		}
		if n, ok := tpl.Notice(tmplEarly, loc); !ok || n.Title == "" || n.Body == "" {
			t.Errorf("Notice(%s) = %+v, %v", loc, n, ok)
		}
	}
}

// keys returns the keys of a map (test messages).
func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
