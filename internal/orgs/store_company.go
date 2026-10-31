package orgs

import (
	"context"
	"uuid"
)

// companyStore is the company profile of a company org (02 §7.2;
// company.go, manage.go, read.go).
type companyStore interface {
	// InsertCompany writes the company profile of an org; org_kind is the
	// parent org's kind (a non-company org fails the CHECK: 422).
	InsertCompany(ctx context.Context, c Company) error
	// GetCompany returns the company profile of orgID or ErrNotFound.
	GetCompany(ctx context.Context, orgID uuid.UUID) (Company, error)
	// CompaniesOf returns the company profiles of the orgs among orgIDs that
	// have one (adminListOrgs' review facts, BE-165; cross-org).
	CompaniesOf(ctx context.Context, orgIDs []uuid.UUID) (map[uuid.UUID]Company, error)
	// UpdateCompany writes the profile columns (sector, size band, e-mail
	// domain and its verification, HQ city, tax identity) of orgID's
	// company and returns the stored row.
	UpdateCompany(ctx context.Context, c Company) (Company, error)
}
