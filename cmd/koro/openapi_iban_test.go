package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

// fullIBANAllowed are the only places a success response may carry a full
// IBAN (BE-129: data minimisation and payment security), with the reason.
// An operation row allows its whole response; a schema row the schema
// wherever it appears.
var fullIBANAllowed = map[string]string{
	// The audited reveals: one number, the access reason required, every
	// call recorded like a break-glass read (parties.RevealIBAN).
	"op:adminRevealBankAccountIban":       "audited reveal",
	"op:adminRevealPayoutIban":            "audited reveal",
	"op:adminRevealCollectionAccountIban": "audited reveal",
	"op:adminRevealIncomingTransferIban":  "audited reveal",
	"op:adminRevealRefundIban":            "audited reveal",
	// The buyer's payment instructions: whoever pays types the number at
	// their bank — Koro's collection account, and in model C the approved
	// account of a legal-person payee (06 H-54).
	"schema:PaymentInstructionsDTO": "the payer's instruction",
	"schema:DirectPaymentDTO":       "the payer's instruction",
}

// fullIBANField reports a member that holds a full IBAN: iban, *_iban or
// iban_* other than the masked form.
func fullIBANField(name string) bool {
	n := strings.ToLower(name)
	if strings.HasSuffix(n, "_masked") {
		return false
	}
	return n == "iban" || strings.HasSuffix(n, "_iban") || strings.HasPrefix(n, "iban_")
}

// TestOpenAPI_NoFullIBANInResponses (BE-129): no list or detail response of
// the full API carries a full IBAN — every finance response shows
// iban_masked (or *_iban_masked) — outside the reasoned allowlist, and
// every allowlist row still names a registered operation or schema.
// Request bodies are not checked: entering an IBAN is the input.
func TestOpenAPI_NoFullIBANInResponses(t *testing.T) {
	t.Parallel()
	oapi := specApp(t)
	var v []string
	ops := map[string]bool{}
	for path, item := range oapi.Paths {
		for method, o := range map[string]*huma.Operation{"GET": item.Get, "POST": item.Post, "PUT": item.Put,
			"PATCH": item.Patch, "DELETE": item.Delete} {
			if o == nil {
				continue
			}
			ops[o.OperationID] = true
			if fullIBANAllowed["op:"+o.OperationID] != "" {
				continue
			}
			for status, r := range o.Responses {
				if r == nil || !strings.HasPrefix(status, "2") {
					continue
				}
				for _, mt := range r.Content {
					if mt == nil {
						continue
					}
					w := ibanWalker{oapi: oapi, seen: map[*huma.Schema]bool{},
						at: fmt.Sprintf("%s %s (%s) %s", method, path, o.OperationID, status)}
					w.walk(mt.Schema, "body")
					v = append(v, w.v...)
				}
			}
		}
	}
	slices.Sort(v)
	if len(v) > 0 {
		t.Fatalf("full IBANs in responses (show iban_masked; reveal through an audited operation):\n%s", strings.Join(v, "\n"))
	}
	for row := range fullIBANAllowed {
		kind, name, _ := strings.Cut(row, ":")
		if (kind == "op" && !ops[name]) || (kind == "schema" && oapi.Components.Schemas.Map()[name] == nil) {
			t.Errorf("allowlist row %s names nothing registered", row)
		}
	}
	for _, reveal := range []string{"adminRevealBankAccountIban", "adminRevealPayoutIban", "adminRevealCollectionAccountIban",
		"adminRevealIncomingTransferIban", "adminRevealRefundIban"} {
		o := operationByID(oapi, reveal)
		if o == nil || o.Extensions["x-koro-rate-limit"] != "break_glass" || !strings.Contains(o.Description, "X-Koro-Access-Reason") ||
			!slices.ContainsFunc(o.Parameters, func(p *huma.Param) bool { return p.In == "header" && p.Name == "X-Koro-Access-Reason" }) {
			t.Errorf("%s: not a reveal with the access reason under the break_glass rate class", reveal)
		}
	}
}

// operationByID returns the operation of oapi with the operationId, or nil.
func operationByID(oapi *huma.OpenAPI, id string) *huma.Operation {
	for _, item := range oapi.Paths {
		for _, o := range []*huma.Operation{item.Get, item.Post, item.Put, item.Patch, item.Delete} {
			if o != nil && o.OperationID == id {
				return o
			}
		}
	}
	return nil
}

// ibanWalker walks a response schema, following each $ref once.
type ibanWalker struct {
	oapi *huma.OpenAPI
	seen map[*huma.Schema]bool
	at   string
	v    []string
}

func (w *ibanWalker) walk(s *huma.Schema, where string) {
	if s == nil {
		return
	}
	if s.Ref != "" {
		name := strings.TrimPrefix(s.Ref, "#/components/schemas/")
		if fullIBANAllowed["schema:"+name] != "" {
			return
		}
		s = w.oapi.Components.Schemas.SchemaFromRef(s.Ref)
		where = name
	}
	if s == nil || w.seen[s] {
		return
	}
	w.seen[s] = true
	for prop, ps := range s.Properties {
		if fullIBANField(prop) {
			w.v = append(w.v, fmt.Sprintf("%s: %s.%s", w.at, where, prop))
		}
		w.walk(ps, where+"."+prop)
	}
	w.walk(s.Items, where+"[]")
	for _, list := range [][]*huma.Schema{s.AllOf, s.AnyOf, s.OneOf} {
		for _, sub := range list {
			w.walk(sub, where)
		}
	}
}
