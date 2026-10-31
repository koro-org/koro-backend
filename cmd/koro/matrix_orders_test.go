package main

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
	"uuid"

	"koro/internal/catalog"
	"koro/internal/files"
	"koro/internal/orders"
	"koro/internal/platform/config"
)

// matrixCollectionAccount is Koro's collection account of the matrix
// (hooks.collectionAccount until payments, BE-041, provides it).
var matrixCollectionAccount = hookCollectionAccount{ID: uuid.MustParse("6b6f726f-0040-4000-8000-000000000001"),
	IBAN: "TR330006100519786457841326", HolderName: "Koro Teknoloji A.Ş.", BankName: "Örnek Bank"}

// matrixFixtures seeds fixtures/orders.yaml (03 §7.3) into the in-memory
// stores installed by useMemStore:
//
//	kind offering  an active offering of org (service, payee) without a stock or a per-buyer
//	               limit, seeded into catalog's store (the orders' own lines refer to it)
//	kind order     org, party, placed_by, status (awaiting_payment | paid | in_fulfilment | disputed),
//	               placed at matrixOrgTime, expiring 7 days later, paid a day later
//	kind line      order, offering, fulfillment (pending | marked | disputed), marked_by, evidence
//	               (a file fixture, optional): one unit of 1,000,000 kuruş + 20 % VAT under
//	               policy_default (10 % from the seller)
//	kind file      a completed delivery_evidence upload (BE-093) of org by uploaded_by in the files
//	               store (scan skipped: served while FILES_SCAN_REQUIRED is false), attached to the
//	               line fixture of its line field when given
//
// It also installs Koro's collection account when payments did not. The
// fixtures are seeded only under the matrix config orders_seed: true (the
// orders operations and scenarios set it): open orders of company_a would
// block the orgs module's closeOrg of company_a (hooks.openCommerce).
func (w *ordersWiring) matrixFixtures(fx *matrixFixtures) error {
	if seed, _ := matrixOrderSeeds.Load(w); seed != true {
		return nil
	}
	ctx := context.Background()
	mem, ok := w.built.(*orders.MemStore)
	if !ok {
		return fmt.Errorf("orders: the matrix needs the in-memory store, got %T", w.built)
	}
	cat, ok := w.late.catalog.built.(*catalog.MemStore)
	if !ok {
		return fmt.Errorf("orders: the matrix needs catalog's in-memory store, got %T", w.late.catalog.built)
	}
	if w.late.hooks.collectionAccount == nil {
		w.late.hooks.collectionAccount = func(context.Context) (hookCollectionAccount, bool, error) {
			return matrixCollectionAccount, true, nil
		}
	}
	all := fx.Of("orders")
	rank := map[string]int{"offering": 0, "file": 0, "order": 1, "line": 2}
	slices.SortStableFunc(all, func(a, b fixture) int { return cmp.Compare(rank[a.Kind], rank[b.Kind]) })
	sold := map[string]int{} // offering fixture → units on the fixture lines
	for _, f := range all {
		if name, _ := f.Fields["offering"].(string); f.Kind == "line" {
			sold[name]++
		}
	}
	lines := map[uuid.UUID][]fixture{}
	var ordersOf []fixture
	for _, f := range all {
		switch f.Kind {
		case "offering":
			if err := matrixSoldOffering(ctx, fx, cat, f, sold[f.Name]); err != nil {
				return err
			}
		case "file":
			if err := w.matrixEvidence(ctx, fx, f); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "order":
			ordersOf = append(ordersOf, f)
		case "line":
			o, err := fx.Ref(f, "order")
			if err != nil {
				return err
			}
			lines[o] = append(lines[o], f)
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	for _, f := range ordersOf {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		o, err := matrixOrder(fx, f, id)
		if err != nil {
			return err
		}
		var ls []orders.Line
		for _, lf := range lines[id] {
			l, err := matrixLine(fx, lf, o)
			if err != nil {
				return err
			}
			ls = append(ls, l)
			o.SubtotalAmount += l.NetAmount
			o.VATAmount += l.VATAmount
			o.TotalAmount += l.GrossAmount
			o.PayableAmount += l.PayableAmount
		}
		o.KoroCollectAmount = o.PayableAmount
		if o.Status.PaidState() {
			o.PaidAmount = o.KoroCollectAmount
		}
		if err := mem.InsertOrder(ctx, o, ls); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	return nil
}

// matrixEvidence seeds a delivery_evidence upload (kind file; BE-093;
// scan: infected seeds one the scan rejected after the upload, BE-154).
func (w *ordersWiring) matrixEvidence(ctx context.Context, fx *matrixFixtures, f fixture) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	org, err := fx.Ref(f, "org")
	if err != nil {
		return err
	}
	by, err := fx.Ref(f, "uploaded_by")
	if err != nil {
		return err
	}
	line, err := optRef(fx, f, "line")
	if err != nil {
		return err
	}
	objects, err := w.late.files.memObjects()
	if err != nil {
		return err
	}
	size, mime := int64(len(matrixFilePDF)), files.MIMEPDF
	row := files.File{
		ID: id, Purpose: files.PurposeDeliveryEvidence, OrgID: &org, UploadedBy: &by, Bucket: testBuckets.Files,
		StorageKey: "matrix/" + f.Name, DeclaredMIME: mime, DetectedMIME: &mime, SizeBytes: &size, SHA256: make([]byte, 32),
		Status: files.StatusActive, ScanStatus: files.ScanSkipped,
	}
	if scan, ok := f.Fields["scan"].(string); ok {
		row.ScanStatus = files.ScanStatus(scan)
	}
	if line != nil {
		module, at := "orders", matrixOrgTime.Add(24*time.Hour)
		row.AttachedModule, row.AttachedEntityID, row.AttachedAt = &module, line, &at
	}
	objects.Upload(row.Bucket, row.StorageKey, matrixFilePDF, mime)
	return w.late.files.store.Insert(ctx, row)
}

func matrixSoldOffering(ctx context.Context, fx *matrixFixtures, cat *catalog.MemStore, f fixture, sold int) error {
	id, err := fx.ID(f.Name)
	if err != nil {
		return err
	}
	org, err := fx.Ref(f, "org")
	if err != nil {
		return err
	}
	svc, err := fx.Ref(f, "service")
	if err != nil {
		return err
	}
	payee, err := fx.Ref(f, "payee")
	if err != nil {
		return err
	}
	by, err := fx.ID("club_admin")
	if err != nil {
		return err
	}
	approver, err := fx.ID("platform_admin_2")
	if err != nil {
		return err
	}
	t := matrixOrgTime
	o := catalog.Offering{ID: id, ServiceID: svc, SellerKind: catalog.SellerOrg, OrgID: org, DefaultPayeeID: &payee,
		Title: "Matris " + f.Name, Deliverables: catalog.Deliverables{{Key: "booth_3x3", Label: "3x3 stant alanı"}},
		UnitPriceAmount: 1_000_000, Currency: "TRY", VATRateBP: 2000, CompensationType: "cash", QuantitySold: sold,
		InvoiceTrigger: catalog.InvoiceOnFulfillment, UniversityApprovalStatus: catalog.UniNotRequired, Status: catalog.OfferingActive,
		SubmittedAt: &t, ApprovedBy: &approver,
		ApprovedAt: &t, CreatedBy: by, CreatedAt: t, UpdatedAt: t}
	if _, err := cat.InsertOffering(ctx, o); err != nil {
		return fmt.Errorf("fixture %s: %w", f.Name, err)
	}
	return nil
}

func matrixOrder(fx *matrixFixtures, f fixture, id uuid.UUID) (orders.Order, error) {
	org, err := fx.Ref(f, "org")
	if err != nil {
		return orders.Order{}, err
	}
	party, err := fx.Ref(f, "party")
	if err != nil {
		return orders.Order{}, err
	}
	by, err := fx.Ref(f, "placed_by")
	if err != nil {
		return orders.Order{}, err
	}
	status, _ := f.Fields["status"].(string)
	t := matrixOrgTime
	o := orders.Order{ID: id, OrderNo: matrixOrderNo(id), OrgID: org, OrgKind: "company", PlacedByUserID: by, BuyerPartyID: party,
		BuyerSnapshot: orders.BuyerSnapshot{Title: "Matris Şirketi A.Ş.", TaxOffice: "Kozyatağı", TaxNo: "1234567890"},
		ModelCode:     orders.ModelA, CollectionMethod: orders.CollectBankTransfer, Status: orders.Status(status), Currency: "TRY",
		ExpiresAt: t.Add(orders.DefaultPaymentWindow), PlacedAt: t}
	if o.Status.PaidState() {
		paid := t.Add(24 * time.Hour)
		o.PaidAt = &paid
	}
	return o, nil
}

// matrixOrderNo is a stable reference code of a fixture order (KR + 8
// Crockford characters from its ID).
func matrixOrderNo(id uuid.UUID) string {
	const alpha = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	b := make([]byte, 8)
	for i := range b {
		b[i] = alpha[int(id[i])%len(alpha)]
	}
	return "KR" + string(b)
}

func matrixLine(fx *matrixFixtures, f fixture, o orders.Order) (orders.Line, error) {
	id, err := fx.ID(f.Name)
	if err != nil {
		return orders.Line{}, err
	}
	off, err := fx.Ref(f, "offering")
	if err != nil {
		return orders.Line{}, err
	}
	seller, err := fx.ID("club_a")
	if err != nil {
		return orders.Line{}, err
	}
	payee, err := fx.ID("payee_club_a")
	if err != nil {
		return orders.Line{}, err
	}
	svc, err := fx.ID("svc_booth")
	if err != nil {
		return orders.Line{}, err
	}
	policy, err := fx.ID("policy_default")
	if err != nil {
		return orders.Line{}, err
	}
	status, _ := f.Fields["fulfillment"].(string)
	t := matrixOrgTime
	l := orders.Line{ID: id, OrderID: o.ID, OrgID: o.OrgID, SellerOrgID: seller, PayeeID: &payee, OfferingID: off, ServiceID: svc,
		CommercePolicyID: policy, TitleSnapshot: "Matris stant paketi", DeliverablesSnapshot: orders.Deliverables{{Key: "booth_3x3", Label: "3x3 stant alanı"}},
		Quantity: 1, UnitPriceAmount: 1_000_000, NetAmount: 1_000_000, VATRateBP: 2000, VATAmount: 200_000, GrossAmount: 1_200_000,
		CommissionRateBP: 1000, CommissionBasis: "net_excl_vat", CommissionPayer: "seller", CommissionAmount: 100_000,
		CommissionVATRateBP: 2000, CommissionVATAmount: 20_000, DisputeWindowDays: 7, SellerNetAmount: 1_080_000,
		PayableAmount: 1_200_000, FulfillmentStatus: orders.Fulfillment(status)}
	if l.FulfillmentStatus == orders.FulfillMarked || l.FulfillmentStatus == orders.FulfillDisputed {
		by, err := fx.Ref(f, "marked_by")
		if err != nil {
			return orders.Line{}, err
		}
		marked := t.Add(24 * time.Hour)
		ends := marked.AddDate(0, 0, 7)
		l.FulfillmentMarkedAt, l.FulfillmentMarkedBy, l.DisputeWindowEndsAt = &marked, &by, &ends
		if l.FulfillmentStatus == orders.FulfillDisputed {
			why := "Matris itirazı"
			l.DisputeReason = &why
		}
	}
	evidence, err := optRef(fx, f, "evidence")
	if err != nil {
		return orders.Line{}, err
	}
	l.FulfillmentEvidenceFileID = evidence
	return l, nil
}

// matrixOrderSeeds holds the orders_seed config of each matrix app's
// wiring (the wiring struct is not the matrix's to extend).
var matrixOrderSeeds sync.Map

// matrixConfig applies orders_seed (bool): seed fixtures/orders.yaml.
func (w *ordersWiring) matrixConfig(_ *config.Modules, key string, v any) (bool, error) {
	if key != "orders_seed" {
		return false, nil
	}
	seed, ok := v.(bool)
	if !ok {
		return true, fmt.Errorf("orders_seed %v: want true or false", v)
	}
	matrixOrderSeeds.Store(w, seed)
	return true, nil
}

// matrixHook runs the orders scenario hooks: no_collection_account removes
// Koro's collection account (hooks.collectionAccount).
func (w *ordersWiring) matrixHook(_ *matrixFixtures, name string, _ map[string]any) error {
	if name != "no_collection_account" {
		return fmt.Errorf("unknown orders hook %q", name)
	}
	w.late.hooks.collectionAccount = nil
	return nil
}
