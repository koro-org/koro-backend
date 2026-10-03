package kvkk

import (
	"context"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/problem"
)

// The sub-processor inventory (alt işleyen envanteri, 02 §16.5): where
// personal data goes and on which transfer basis (KVKK md. 9). Retired by
// active = false, never deleted.

const (
	maxSubName    = 200
	maxSubService = 200
	maxSubRegion  = 100
	maxSubNotes   = 2000
	resourceSub   = "subprocessor"
)

var countryRe = regexp.MustCompile(`^[A-Z]{2}$`)

// SubprocessorInput is the body of adminCreateSubprocessor (02 §16.5).
type SubprocessorInput struct {
	Name                string
	Service             string
	DataCategories      []string
	Region              string
	CountryCode         string
	TransferMechanism   string
	ContractSignedAt    *civil.Date
	AuthorityNotifiedAt *civil.Date
	Notes               string
}

// SubprocessorPatch is the body of adminUpdateSubprocessor; nil fields stay.
type SubprocessorPatch struct {
	Name                *string
	Service             *string
	DataCategories      *[]string
	Region              *string
	CountryCode         *string
	TransferMechanism   *string
	ContractSignedAt    *civil.Date
	AuthorityNotifiedAt *civil.Date
	Active              *bool
	Notes               *string
}

func subEvent(a authz.Actor, x Subprocessor, meta map[string]any) audit.Event {
	id := x.ID
	m := map[string]any{"active": x.Active, "transfer_mechanism": x.TransferMechanism, "country_code": x.CountryCode}
	for k, v := range meta {
		m[k] = v
	}
	return audit.Event{Action: string(authz.KvkkSubprocessorManage), Outcome: audit.Success, ResourceType: resourceSub,
		ResourceID: &id, Metadata: m}.By(a)
}

// checkSub validates a whole sub-processor (the 000093 CHECKs first, so the
// answer is a field error rather than a constraint).
func (s *service) checkSub(x *Subprocessor) error {
	var err error
	if x.Name, err = cleanText(x.Name, "body.name", maxSubName, true); err != nil {
		return err
	}
	if x.Service, err = cleanText(x.Service, "body.service", maxSubService, true); err != nil {
		return err
	}
	if x.Region, err = cleanText(x.Region, "body.region", maxSubRegion, true); err != nil {
		return err
	}
	if x.Notes, err = cleanText(x.Notes, "body.notes", maxSubNotes, false); err != nil {
		return err
	}
	if x.DataCategories, err = categories(x.DataCategories, "body.data_categories", true); err != nil {
		return err
	}
	x.CountryCode = strings.ToUpper(strings.TrimSpace(x.CountryCode))
	today := civil.Today(s.now())
	switch {
	case !countryRe.MatchString(x.CountryCode):
		return invalid("body.country_code", "ISO 3166-1 alpha-2 code")
	case !slices.Contains(transferMechanisms, x.TransferMechanism):
		return invalid("body.transfer_mechanism", "domestic, standard_contract, adequacy, explicit_consent or undecided")
	case x.TransferMechanism == "domestic" && x.CountryCode != "TR":
		return invalid("body.transfer_mechanism", "a processor abroad is never domestic (KVKK md. 9)")
	case x.ContractSignedAt != nil && (!x.ContractSignedAt.IsValid() || x.ContractSignedAt.After(today)):
		return invalid("body.contract_signed_at", "a valid date, not in the future")
	case x.AuthorityNotifiedAt != nil && (!x.AuthorityNotifiedAt.IsValid() || x.AuthorityNotifiedAt.After(today)):
		return invalid("body.authority_notified_at", "a valid date, not in the future")
	}
	return nil
}

// ListSubprocessors implements Service (adminListSubprocessors).
func (s *service) ListSubprocessors(ctx context.Context, a authz.Actor, active *bool) ([]Subprocessor, error) {
	if err := authz.Can(ctx, a, authz.KvkkSubprocessorList, adminSurface); err != nil {
		return nil, err
	}
	return s.d.Records.Subprocessors(ctx, active)
}

// CreateSubprocessor implements Service (adminCreateSubprocessor).
func (s *service) CreateSubprocessor(ctx context.Context, a authz.Actor, in SubprocessorInput) (Subprocessor, error) {
	if err := authz.Can(ctx, a, authz.KvkkSubprocessorManage, adminSurface); err != nil {
		return Subprocessor{}, err
	}
	x := Subprocessor{ID: uuid.NewV7(), Name: in.Name, Service: in.Service, DataCategories: in.DataCategories,
		Region: in.Region, CountryCode: in.CountryCode, TransferMechanism: in.TransferMechanism,
		ContractSignedAt: in.ContractSignedAt, AuthorityNotifiedAt: in.AuthorityNotifiedAt, Active: true, Notes: in.Notes}
	if err := s.checkSub(&x); err != nil {
		return Subprocessor{}, err
	}
	hash, err := idempotency.Hash(in)
	if err != nil {
		return Subprocessor{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (Subprocessor, error) {
		if err := s.d.Records.InsertSubprocessor(ctx, x); err != nil {
			return Subprocessor{}, err
		}
		if err := s.d.Audit.Log(ctx, subEvent(a, x, map[string]any{"step": "created"})); err != nil {
			return Subprocessor{}, err
		}
		return s.d.Records.Subprocessor(ctx, x.ID)
	})
	if err != nil {
		return Subprocessor{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "sub-processor added", slog.String("subprocessor_id", out.ID.String()))
	return out, nil
}

// UpdateSubprocessor implements Service (adminUpdateSubprocessor, If-Match).
func (s *service) UpdateSubprocessor(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p SubprocessorPatch) (Subprocessor, error) {
	if err := authz.Can(ctx, a, authz.KvkkSubprocessorManage, adminSurface); err != nil {
		return Subprocessor{}, err
	}
	if p == (SubprocessorPatch{}) {
		return Subprocessor{}, invalid("body", "nothing to update")
	}
	if ifMatch == "" {
		return Subprocessor{}, problem.ErrPreconditionRequired
	}
	var out Subprocessor
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		x, err := s.d.Records.LockSubprocessor(ctx, id)
		if err != nil {
			return err
		}
		if !ifMatches(ifMatch, x.UpdatedAt) {
			return problem.ErrPreconditionFailed
		}
		var changed []string
		str := func(dst *string, v *string, name string) {
			if v != nil {
				*dst = *v
				changed = append(changed, name)
			}
		}
		str(&x.Name, p.Name, "name")
		str(&x.Service, p.Service, "service")
		str(&x.Region, p.Region, "region")
		str(&x.CountryCode, p.CountryCode, "country_code")
		str(&x.TransferMechanism, p.TransferMechanism, "transfer_mechanism")
		str(&x.Notes, p.Notes, "notes")
		if p.DataCategories != nil {
			x.DataCategories = *p.DataCategories
			changed = append(changed, "data_categories")
		}
		if p.ContractSignedAt != nil {
			v := *p.ContractSignedAt
			x.ContractSignedAt = &v
			changed = append(changed, "contract_signed_at")
		}
		if p.AuthorityNotifiedAt != nil {
			v := *p.AuthorityNotifiedAt
			x.AuthorityNotifiedAt = &v
			changed = append(changed, "authority_notified_at")
		}
		if p.Active != nil {
			x.Active = *p.Active
			changed = append(changed, "active")
		}
		if err := s.checkSub(&x); err != nil {
			return err
		}
		if err := s.d.Records.UpdateSubprocessor(ctx, x); err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, subEvent(a, x, map[string]any{"changed": strings.Join(changed, ",")})); err != nil {
			return err
		}
		out, err = s.d.Records.Subprocessor(ctx, id)
		return err
	})
	if err != nil {
		return Subprocessor{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "sub-processor updated", slog.String("subprocessor_id", id.String()))
	return out, nil
}
