package catalog

import (
	"context"
	"fmt"
	"uuid"
)

// InvoiceTerms is how billing invoices an order line of an offering
// (BE-042; 01 §4.21): when the service invoice is planned (the offering's
// invoice_trigger, which never changes once the offering is on sale), the
// text and the VAT codes of the invoice line (the service's) and whether
// the invoice is uploaded or issued through the provider (the frozen
// commerce policy's invoice_automation).
type InvoiceTerms struct {
	OfferingID         uuid.UUID
	Trigger            InvoiceTrigger
	Description        *string // services.invoice_description_tr; nil → the line's title
	VATExemptionCode   *string
	VATWithholdingCode *string
	Automation         InvoiceAutomation
}

// InvoiceTerms implements Offerings: a read of the offering, its service
// and the policy, whatever their status.
func (s *service) InvoiceTerms(ctx context.Context, offeringID, policyID uuid.UUID) (InvoiceTerms, error) {
	o, err := s.d.Store.GetOffering(ctx, offeringID, false)
	if err != nil {
		return InvoiceTerms{}, err
	}
	sv, err := s.d.Store.GetService(ctx, o.ServiceID, false)
	if err != nil {
		return InvoiceTerms{}, fmt.Errorf("catalog: service of offering %s: %w", o.ID, err)
	}
	p, err := s.d.Store.GetPolicy(ctx, policyID, false)
	if err != nil {
		return InvoiceTerms{}, err
	}
	return InvoiceTerms{
		OfferingID: o.ID, Trigger: o.InvoiceTrigger, Description: sv.InvoiceDescriptionTR,
		VATExemptionCode: sv.VATExemptionCode, VATWithholdingCode: sv.VATWithholdingCode, Automation: p.InvoiceAutomation,
	}, nil
}
