package authz_test

import (
	"testing"
	"uuid"

	"koro/internal/authz"
)

func TestOrgResource_CarriesMandatoryOrgAttrs(t *testing.T) {
	t.Parallel()
	id := uuid.NewV7()
	r := authz.OrgResource(id, "company", "active")
	if r.Type != authz.TypeOrg || r.ID != id || r.OrgID == nil || *r.OrgID != id {
		t.Fatalf("resource = %+v", r)
	}
	if r.Attr("org_kind") != "company" || r.Attr("org_status") != "active" || len(r.Attrs) != 2 {
		t.Fatalf("Attrs = %v", r.Attrs)
	}
	other := authz.OrgResource(uuid.NewV7(), "club", "active")
	if *other.OrgID == id {
		t.Fatal("OrgID aliases another resource's ID")
	}
}
