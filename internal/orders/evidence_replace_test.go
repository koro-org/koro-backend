package orders_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/files/filestest"
	"koro/internal/notify"
	"koro/internal/orders"
	"koro/internal/platform/problem"
	"koro/internal/platform/problem/problemtest"
)

// rejectFile takes the file out of use after its upload, as the scan does
// when it finds malware (BE-048: the attached row stays, infected).
func (e *env) rejectFile(t *testing.T, id uuid.UUID) {
	t.Helper()
	f, err := e.fileStore.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.ScanStatus = files.ScanInfected
	if err := e.fileStore.Update(t.Context(), f); err != nil {
		t.Fatal(err)
	}
}

// TestOrders_RejectedEvidence_StateNoticeReplace (BE-154, T-393, Q-37): a
// line's delivery evidence shows its file state (clean while it is served)
// to the buyer, on the seller's sales list and after a delivery action;
// replacing an evidence that was not rejected is 409 files.not_rejected.
// Once the scan rejects it the views read rejected (the download stays
// 409); file.rejected tells the seller's markers (the org's sales link)
// and, the line being disputed, the finance admins (the admin order link),
// one notice per recipient however often it is delivered. Only the
// seller's markers replace it (the buyer's admin 403, another club 404, a
// file of another purpose 422); the new file is attached to the line, the
// old row stays attached (audit: both IDs and the reason), the delivery
// and the dispute do not change, the state is clean again and the evidence
// is downloaded. A late file.rejected of the replaced file sends nothing;
// a line marked without evidence has none to replace (409).
func TestOrders_RejectedEvidence_StateNoticeReplace(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	l := e.paidLine(t)
	old := e.evidenceFile(t, "club_a", "club_admin", nil)
	if _, err := e.markWith(t, "club_admin", l.ID, old); err != nil {
		t.Fatal(err)
	}
	buyer, club := actor(t, "company_admin"), actor(t, "club_admin")
	state := func(want files.State) {
		t.Helper()
		v, err := e.svc.GetOrder(ctxOf(t, buyer), buyer, l.OrderID)
		if err != nil || files.StateOf(v.EvidenceStates, &old) != want {
			t.Fatalf("buyer's order evidence states = %+v, %v; want %s", v.EvidenceStates, err, want)
		}
		p, err := e.svc.ListOrgSales(ctxOf(t, club), club, org("club_a"), orders.SalesQuery{})
		if err != nil || files.StateOf(p.EvidenceStates, &old) != want {
			t.Fatalf("sales evidence states = %+v, %v; want %s", p.EvidenceStates, err, want)
		}
	}
	state(files.StateClean)
	fresh := e.evidenceFile(t, "club_a", "club_admin", nil)
	_, err := e.svc.ReplaceLineEvidence(ctxOf(t, club), club, l.ID, fresh)
	problemtest.WantErr(t, err, files.ErrNotRejected)

	disputed, err := e.svc.DisputeLine(ctxOf(t, buyer), buyer, l.ID, "Stant kurulmadı")
	if err != nil || disputed.EvidenceState != files.StateClean {
		t.Fatalf("dispute = %+v, %v; want the evidence state clean", disputed, err)
	}
	e.rejectFile(t, old)
	state(files.StateRejected)
	_, err = e.svc.LineEvidence(ctxOf(t, buyer), buyer, l.ID)
	problemtest.WantProblem(t, err, 409, "files.not_clean")

	for range 2 { // at least once
		if err := filestest.Reject(t, e.mod.Subscribe, orders.NotifyRejectedEvidenceTaskName, old, files.PurposeDeliveryEvidence,
			files.RejectInfected, "orders", l.ID); err != nil {
			t.Fatal(err)
		}
	}
	o := e.order(t, l.OrderID)
	want := map[string]string{
		"club_admin":      notify.OrgSalesLink(org("club_a")).String(),
		"finance_admin":   notify.AdminOrderLink(o.OrderNo).String(),
		"finance_admin_2": notify.AdminOrderLink(o.OrderNo).String(),
	}
	got := map[uuid.UUID][]string{} // recipient → links
	for _, m := range e.sender.all() {
		if m.Template == "orders.delivery_evidence_rejected" {
			got[*m.UserID] = append(got[*m.UserID], m.LinkPath.String())
		}
	}
	if len(got) != len(want) {
		t.Fatalf("evidence notices %v, want %v", got, want)
	}
	for name, link := range want {
		if links := got[authztest.ID(name)]; len(links) != 1 || links[0] != link {
			t.Fatalf("%s: notices %v; want one linking %s", name, links, link)
		}
	}

	_, err = e.svc.ReplaceLineEvidence(ctxOf(t, buyer), buyer, l.ID, fresh)
	problemtest.WantErr(t, err, authz.ErrForbidden)
	other := actor(t, "other_club_admin")
	_, err = e.svc.ReplaceLineEvidence(ctxOf(t, other), other, l.ID, fresh)
	problemtest.WantErr(t, err, authz.ErrNotFound)
	post := e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.Purpose = files.PurposePostMedia })
	_, err = e.svc.ReplaceLineEvidence(ctxOf(t, club), club, l.ID, post)
	problemtest.WantErr(t, err, problem.ErrValidation)

	v, err := e.svc.ReplaceLineEvidence(ctxOf(t, club), club, l.ID, fresh)
	if err != nil || v.Line.FulfillmentEvidenceFileID == nil || *v.Line.FulfillmentEvidenceFileID != fresh ||
		v.EvidenceState != files.StateClean || v.Line.FulfillmentStatus != orders.FulfillDisputed || !v.SellerView {
		t.Fatalf("replaced = %+v, %v", v, err)
	}
	for _, id := range []uuid.UUID{old, fresh} {
		f, err := e.files.Get(t.Context(), id)
		if err != nil || f.AttachedModule != "orders" || f.AttachedEntityID != l.ID {
			t.Fatalf("file %s after the replacement = %+v, %v; the old and the new belong to the line", id, f, err)
		}
	}
	recs := e.audit.Of(authz.OrdersLineMarkFulfilled)
	last := recs[len(recs)-1].Metadata
	if last["operation"] != "replace_rejected_file" || last["replaced_file_id"] != old.String() ||
		last["file_id"] != fresh.String() || last["replaced_reason"] != "infected" {
		t.Fatalf("replace audit = %+v", last)
	}
	if dl, err := e.svc.LineEvidence(ctxOf(t, buyer), buyer, l.ID); err != nil || dl.URL == "" {
		t.Fatalf("evidence after the replacement = %+v, %v", dl, err)
	}
	sent := len(e.sender.all())
	if err := filestest.Reject(t, e.mod.Subscribe, orders.NotifyRejectedEvidenceTaskName, old, files.PurposeDeliveryEvidence,
		files.RejectInfected, "orders", l.ID); err != nil || len(e.sender.all()) != sent {
		t.Fatalf("late file.rejected of the replaced evidence: %v, %d new notices", err, len(e.sender.all())-sent)
	}
	_, err = e.svc.ReplaceLineEvidence(ctxOf(t, club), club, l.ID, e.evidenceFile(t, "club_a", "club_admin", nil))
	problemtest.WantErr(t, err, files.ErrNotRejected)

	plain := e.paidLine(t)
	e.mark(t, plain.ID)
	_, err = e.svc.ReplaceLineEvidence(ctxOf(t, club), club, plain.ID, e.evidenceFile(t, "club_a", "club_admin", nil))
	problemtest.WantErr(t, err, orders.ErrInvalidState)
}
