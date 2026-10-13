package catalog

import (
	"context"
	"errors"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/events"
	"koro/internal/orgs"
)

// OfferingView is an offering as the HTTP operations return it: its
// service, seller and event, the ETag (04 §1.13) and what the caller may
// see. Management is set for the seller's managers and platform staff
// (payee, stock, approval state); a buyer sees none of it (04 §2.14:
// default_payee_id, the policy and the commission never reach the buyer),
// only BuyerFee, the estimated fee of one unit when the policy charges the
// buyer.
type OfferingView struct {
	Offering   Offering
	Service    ServiceItem
	Seller     orgs.OrgSummary
	Event      *events.EventSummary
	ETag       string
	Management bool
	BuyerFee   *int64
}

// OfferingPage is a page of offerings; NextTime / NextBrowse is the
// position of the next page (nil on the last one).
type OfferingPage struct {
	Items      []OfferingView
	NextTime   *TimeKey
	NextBrowse *BrowseKey
	Sort       string
}

// views builds the views of os: services, sellers and events in bulk.
func (s *service) views(ctx context.Context, a authz.Actor, os []Offering) ([]OfferingView, error) {
	svcIDs, orgIDs := make([]uuid.UUID, 0, len(os)), make([]uuid.UUID, 0, len(os))
	for _, o := range os {
		svcIDs, orgIDs = append(svcIDs, o.ServiceID), append(orgIDs, o.OrgID)
	}
	svcs, err := s.d.Store.ServicesByID(ctx, svcIDs)
	if err != nil {
		return nil, err
	}
	sellers, err := s.d.Orgs.GetMany(ctx, orgIDs)
	if err != nil {
		return nil, err
	}
	evs := map[uuid.UUID]*events.EventSummary{}
	out := make([]OfferingView, len(os))
	for i, o := range os {
		v := OfferingView{Offering: o, Service: svcs[o.ServiceID], Seller: sellers[o.OrgID], ETag: etagOf(o.UpdatedAt),
			Management: manager(a, o)}
		if v.Seller.ID == uuid.Nil() {
			v.Seller = orgs.OrgSummary{ID: o.OrgID}
		}
		if o.EventID != nil && s.d.Events != nil {
			e, ok := evs[*o.EventID]
			if !ok {
				got, err := s.d.Events.Get(ctx, *o.EventID)
				switch {
				case errors.Is(err, events.ErrNotFound):
				case err != nil:
					return nil, err
				default:
					e = &got
				}
				evs[*o.EventID] = e
			}
			v.Event = e
		}
		out[i] = v
	}
	return out, nil
}

// view builds one view, with the buyer fee estimate for a non-manager.
func (s *service) view(ctx context.Context, a authz.Actor, o Offering) (OfferingView, error) {
	vs, err := s.views(ctx, a, []Offering{o})
	if err != nil {
		return OfferingView{}, err
	}
	v := vs[0]
	if !v.Management && o.Status == OfferingActive {
		p, err := s.ResolvePolicy(ctx, o.ID, s.now())
		switch {
		case errors.Is(err, ErrNoPolicy):
		case err != nil:
			return OfferingView{}, err
		case p.CommissionPayer == PayerBuyer:
			vat, err := lineVAT(o.UnitPriceAmount, o.VATRateBP)
			if err != nil {
				return OfferingView{}, err
			}
			c, err := p.LineCommission(o.UnitPriceAmount, vat)
			if err != nil {
				return OfferingView{}, err
			}
			v.BuyerFee = &c.BuyerFee
		}
	}
	return v, nil
}

// CheckoutOffering is what an order line needs of an offering on sale
// (ForCheckout; 01 §4.17): the price and VAT of the offering, the VAT
// class and withholding of its service, the seller and its default payee,
// what is delivered and when it is invoiced. orders freezes these onto the
// line (BE-040).
type CheckoutOffering struct {
	ID                  uuid.UUID
	Title               string
	SellerKind          SellerKind
	SellerOrgID         uuid.UUID
	EventID             *uuid.UUID
	DefaultPayeeID      *uuid.UUID // nil for a platform offering
	UnitPriceAmount     int64      // kuruş, excluding VAT
	Currency            string
	VATRateBP           int
	VATClass            VATClass
	VATExemptionCode    *string
	VATWithholdingCode  *string
	VATWithholdingNum   *int16
	VATWithholdingDen   *int16
	InvoiceDescription  *string
	InvoiceTrigger      InvoiceTrigger
	Deliverables        Deliverables
	ServiceID           uuid.UUID
	ServiceCode         string
	Category            Category
	FulfillmentType     FulfillmentType
	EntitlementTemplate JSON // platform-native services: the right the order grants
	PerBuyerLimit       *int
	QuantityRemaining   *int // nil: unlimited
	SalesEndAt          *time.Time
	FulfilBy            *time.Time
}

func checkoutOf(o Offering, sv ServiceItem) CheckoutOffering {
	return CheckoutOffering{
		ID: o.ID, Title: o.Title, SellerKind: o.SellerKind, SellerOrgID: o.OrgID, EventID: o.EventID,
		DefaultPayeeID: o.DefaultPayeeID, UnitPriceAmount: o.UnitPriceAmount, Currency: o.Currency, VATRateBP: o.VATRateBP,
		VATClass: sv.VATClass, VATExemptionCode: sv.VATExemptionCode, VATWithholdingCode: sv.VATWithholdingCode,
		VATWithholdingNum: sv.VATWithholdingNum, VATWithholdingDen: sv.VATWithholdingDen,
		InvoiceDescription: sv.InvoiceDescriptionTR, InvoiceTrigger: o.InvoiceTrigger, Deliverables: o.Deliverables,
		ServiceID: sv.ID, ServiceCode: sv.Code, Category: sv.Category, FulfillmentType: sv.FulfillmentType,
		EntitlementTemplate: sv.EntitlementTemplate, PerBuyerLimit: o.PerBuyerLimit, QuantityRemaining: o.Remaining(),
		SalesEndAt: o.SalesEndAt, FulfilBy: o.FulfilBy,
	}
}
