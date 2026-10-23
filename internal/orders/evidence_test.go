package orders_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/authz/authztest"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/platform/idempotency"
)

// evidenceFile inserts a completed delivery_evidence upload of orgName by
// uploader (scan skipped: served while FILES_SCAN_REQUIRED is false); mut
// edits the row before the insert.
func (e *env) evidenceFile(t *testing.T, orgName, uploader string, mut func(*files.File)) uuid.UUID {
	t.Helper()
	id, o, by, size, mime := uuid.NewV7(), org(orgName), authztest.ID(uploader), int64(2048), files.MIMEJPEG
	f := files.File{ID: id, Purpose: files.PurposeDeliveryEvidence, OrgID: &o, UploadedBy: &by, Bucket: evidenceBuckets.Files,
		StorageKey: "delivery_evidence/2026/10/" + id.String(), DeclaredMIME: mime, DetectedMIME: &mime, SizeBytes: &size,
		SHA256: make([]byte, 32), Status: files.StatusActive, ScanStatus: files.ScanSkipped}
	if mut != nil {
		mut(&f)
	}
	if err := e.fileStore.Insert(t.Context(), f); err != nil {
		t.Fatalf("insert evidence file: %v", err)
	}
	return id
}

// markWith marks the line delivered as persona with the evidence file.
func (e *env) markWith(t *testing.T, persona string, lineID, fileID uuid.UUID) (orders.LineView, error) {
	t.Helper()
	a := actor(t, persona)
	return e.svc.MarkLineFulfilled(ctxOf(t, a), a, lineID, orders.MarkInput{Note: "Stant kuruldu", EvidenceFileID: &fileID})
}

// TestMarkFulfilled_WithEvidence_AttachesFile (BE-093, T-292): the seller
// marks a paid line with a delivery_evidence upload of its own org; the
// line keeps the file, the file is attached to the line (no longer an
// orphan), the mark's audit names it, and an Idempotency-Key replay of the
// same mark answers the same. A platform admin may mark with the seller's
// upload too.
func TestMarkFulfilled_WithEvidence_AttachesFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	l := e.paidLine(t)
	file := e.evidenceFile(t, "club_a", "club_admin", nil)
	club := actor(t, "club_admin")
	ctx := idempotency.WithKey(ctxOf(t, club), idempotency.Key{Scope: club.UserID.String() + ":POST /order-lines/{line_id}/mark-fulfilled",
		Value: "mark-with-evidence-1", Status: 200})
	in := orders.MarkInput{Note: "Stant kuruldu", EvidenceFileID: &file}
	v, err := e.svc.MarkLineFulfilled(ctx, club, l.ID, in)
	if err != nil {
		t.Fatalf("MarkLineFulfilled: %v", err)
	}
	if v.Line.FulfillmentStatus != orders.FulfillMarked || v.Line.FulfillmentEvidenceFileID == nil || *v.Line.FulfillmentEvidenceFileID != file {
		t.Fatalf("marked line = %+v", v.Line)
	}
	if got := e.line(t, l.ID); got.FulfillmentEvidenceFileID == nil || *got.FulfillmentEvidenceFileID != file {
		t.Fatalf("stored line evidence = %v", got.FulfillmentEvidenceFileID)
	}
	f, err := e.files.Get(t.Context(), file)
	if err != nil || f.AttachedModule != "orders" || f.AttachedEntityID != l.ID {
		t.Fatalf("file after the mark = %+v, %v", f, err)
	}
	ev := e.audit.of(authz.OrdersLineMarkFulfilled)
	if len(ev) != 1 || ev[0].Metadata["evidence_file_id"] != file.String() {
		t.Fatalf("mark audit = %+v", ev)
	}
	if again, err := e.svc.MarkLineFulfilled(ctx, club, l.ID, in); err != nil || again.Line.ID != v.Line.ID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	// The confirmation keeps the evidence.
	buyer := actor(t, "company_admin")
	if c, err := e.svc.ConfirmLine(ctxOf(t, buyer), buyer, l.ID); err != nil || c.Line.FulfillmentEvidenceFileID == nil {
		t.Fatalf("confirmed = %+v, %v", c.Line, err)
	}

	// platform_admin marks another line of club_a with a club_a upload.
	l2 := e.paidLine(t)
	pad := e.evidenceFile(t, "club_a", "platform_admin", nil)
	if v, err := e.markWith(t, "platform_admin", l2.ID, pad); err != nil || v.Line.FulfillmentEvidenceFileID == nil {
		t.Fatalf("platform admin mark = %+v, %v", v.Line, err)
	}
}

// TestMarkFulfilled_EvidenceRefused: the evidence must be a completed
// delivery_evidence upload of the line's seller org (422, the same answer
// for an unknown file, another purpose and another org's file); a file
// rejected after its upload or found infected is 409, one attached to
// another record 409 files.already_attached; without the files module the
// mark with evidence fails closed (503). Nothing is marked or attached.
func TestMarkFulfilled_EvidenceRefused(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	other := e.paidLine(t)
	used := e.evidenceFile(t, "club_a", "club_admin", nil)
	if _, err := e.markWith(t, "club_admin", other.ID, used); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		file   func() uuid.UUID
		status int
		code   string
	}{
		{"an unknown file", func() uuid.UUID { return uuid.NewV7() }, 422, "validation_failed"},
		{"another org's upload", func() uuid.UUID { return e.evidenceFile(t, "club_b", "other_club_admin", nil) }, 422, "validation_failed"},
		{"another purpose", func() uuid.UUID {
			return e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.Purpose = files.PurposePartyDocument })
		}, 422, "validation_failed"},
		{"an upload without an org", func() uuid.UUID {
			return e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.OrgID = nil })
		}, 422, "validation_failed"},
		{"an unfinished upload", func() uuid.UUID {
			return e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) {
				f.Status, f.SHA256, f.Bucket = files.StatusUploading, nil, evidenceBuckets.Quarantine
			})
		}, 422, "validation_failed"},
		{"a rejected upload", func() uuid.UUID {
			return e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.Status = files.StatusRejected })
		}, 409, "files.rejected"},
		{"an infected upload", func() uuid.UUID {
			return e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.ScanStatus = files.ScanInfected })
		}, 409, "files.not_clean"},
		{"the evidence of another line", func() uuid.UUID { return used }, 409, "files.already_attached"},
	}
	for _, tt := range tests {
		l := e.paidLine(t)
		file := tt.file()
		_, err := e.markWith(t, "club_admin", l.ID, file)
		if s, c := problemOf(err); s != tt.status || c != tt.code {
			t.Errorf("%s: error = %v (%d %s), want %d %s", tt.name, err, s, c, tt.status, tt.code)
		}
		if got := e.line(t, l.ID); got.FulfillmentStatus != orders.FulfillPending || got.FulfillmentEvidenceFileID != nil {
			t.Errorf("%s: line after the refusal = %s, evidence %v", tt.name, got.FulfillmentStatus, got.FulfillmentEvidenceFileID)
		}
		if f, err := e.files.Get(t.Context(), file); err == nil && f.AttachedModule == "orders" && f.AttachedEntityID == l.ID {
			t.Errorf("%s: the refused file was attached", tt.name)
		}
	}
	// A seller who may not mark the line learns nothing about the file (authz first).
	l := e.paidLine(t)
	file := e.evidenceFile(t, "club_a", "club_admin", nil)
	_, err := e.markWith(t, "other_club_admin", l.ID, file)
	wantErr(t, err, authz.ErrNotFound)

	// Without the files module: the mark with evidence fails closed, a mark without works.
	n := newEnv(t, func(d *orders.Deps) { d.Files = nil })
	nl := n.paidLine(t)
	_, err = n.markWith(t, "club_admin", nl.ID, uuid.NewV7())
	wantErr(t, err, orders.ErrUnavailable)
	n.mark(t, nl.ID)
}

// TestLineEvidence_Download (03 §5.15 orders.line.read_evidence, BE-093):
// the buyer's admins, the seller's markers and the finance and platform
// admins get a 5-minute URL of the evidence and every download is audited;
// the recruiter and support see the line but get 403, other orgs 404; a
// line without evidence is 404; a file the scan has not cleared is 409.
func TestLineEvidence_Download(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	l := e.paidLine(t)
	file := e.evidenceFile(t, "club_a", "club_admin", nil)
	if _, err := e.markWith(t, "club_admin", l.ID, file); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"company_admin", "company_owner_a", "club_admin", "finance_admin", "platform_admin"} {
		a := actor(t, p)
		dl, err := e.svc.LineEvidence(ctxOf(t, a), a, l.ID)
		if err != nil || !strings.Contains(dl.URL, file.String()) || !dl.ExpiresAt.Equal(e.clk.Now().Add(5*time.Minute)) {
			t.Errorf("%s: download = %+v, %v", p, dl, err)
		}
	}
	if ev := e.audit.of(authz.OrdersLineReadEvidence); len(ev) != 5 || ev[0].Metadata["evidence_file_id"] != file.String() ||
		ev[0].ResourceID == nil || *ev[0].ResourceID != l.ID {
		t.Fatalf("download audit = %+v", ev)
	}
	for p, want := range map[string]error{
		"company_member": authz.ErrForbidden, "club_member": authz.ErrForbidden, "support": authz.ErrForbidden,
		"moderator": authz.ErrForbidden, "other_club_admin": authz.ErrNotFound, "other_company_member": authz.ErrNotFound,
		"student": authz.ErrNotFound, "university_rep": authz.ErrNotFound,
	} {
		a := actor(t, p)
		if _, err := e.svc.LineEvidence(ctxOf(t, a), a, l.ID); !errors.Is(err, want) {
			t.Errorf("%s: error = %v, want %v", p, err, want)
		}
	}
	buyer := actor(t, "company_admin")
	plain := e.paidLine(t)
	e.mark(t, plain.ID)
	_, err := e.svc.LineEvidence(ctxOf(t, buyer), buyer, plain.ID)
	wantProblem(t, err, 404, "not_found")
	_, err = e.svc.LineEvidence(ctxOf(t, buyer), buyer, uuid.NewV7())
	wantProblem(t, err, 404, "not_found")

	pending := e.paidLine(t)
	unscanned := e.evidenceFile(t, "club_a", "club_admin", func(f *files.File) { f.ScanStatus = files.ScanPending })
	if _, err := e.markWith(t, "club_admin", pending.ID, unscanned); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.LineEvidence(ctxOf(t, buyer), buyer, pending.ID)
	wantProblem(t, err, 409, "files.not_clean")
	if n := len(e.audit.of(authz.OrdersLineReadEvidence)); n != 5 {
		t.Fatalf("refused downloads were audited: %d events", n)
	}
}

// TestHTTP_MarkWithEvidenceAndDownload: the HTTP surface — the mark takes
// evidence_file_id and answers has_fulfillment_evidence, getOrder shows it
// to the buyer, getOrderLineEvidence answers {url, expires_at}, a recruiter
// gets 403 and a malformed evidence ID 422.
func TestHTTP_MarkWithEvidenceAndDownload(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	app := newHTTP(t, e)
	l := e.paidLine(t)
	file := e.evidenceFile(t, "club_a", "club_admin", nil)
	base := "/api/v1/order-lines/" + l.ID.String()
	rec, _ := call(t, app, "club_admin", http.MethodPost, base+"/mark-fulfilled", map[string]any{"evidence_file_id": "nope"}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed evidence id = %d %s", rec.Code, rec.Body)
	}
	rec, body := call(t, app, "club_admin", http.MethodPost, base+"/mark-fulfilled",
		map[string]any{"note": "Stant kuruldu", "evidence_file_id": file.String()}, nil)
	if rec.Code != http.StatusOK || body["has_fulfillment_evidence"] != true || body["fulfillment_status"] != "marked" {
		t.Fatalf("mark = %d %s", rec.Code, rec.Body)
	}
	rec, body = call(t, app, "company_admin", http.MethodGet, "/api/v1/orders/"+l.OrderID.String(), nil, nil)
	lines, _ := body["lines"].([]any)
	if rec.Code != http.StatusOK || len(lines) != 1 || lines[0].(map[string]any)["has_fulfillment_evidence"] != true {
		t.Fatalf("getOrder = %d %s", rec.Code, rec.Body)
	}
	rec, body = call(t, app, "company_admin", http.MethodGet, base+"/evidence", nil, nil)
	if rec.Code != http.StatusOK || !strings.Contains(body["url"].(string), file.String()) || body["expires_at"] == nil {
		t.Fatalf("evidence = %d %s", rec.Code, rec.Body)
	}
	if rec, _ := call(t, app, "company_member", http.MethodGet, base+"/evidence", nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("recruiter evidence = %d %s", rec.Code, rec.Body)
	}
}
