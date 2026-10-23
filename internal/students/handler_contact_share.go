package students

import (
	"context"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
)

// registerContactShare adds unshareMyAccessGrantContact (04 §2.4; BE-089):
// the student stops sharing the e-mail with one company. It reuses
// students.access_grant.revoke (a partial revocation of the student's own
// grant: the same O cell, no student verification needed).
func registerContactShare(api huma.API, h handler) {
	huma.Register(api, op.Koro(huma.Operation{
		OperationID: "unshareMyAccessGrantContact", Method: http.MethodDelete, Path: "/me/access-grants/{grant_id}/contact",
		Tags: []string{"students"}, DefaultStatus: http.StatusNoContent,
		Summary: "Stop sharing my contact details with a company",
		Description: "The company of this grant stops seeing your e-mail at once; its level 3 access to your profile " +
			"stays (revokeMyAccessGrant ends it). You shared the e-mail with this company only, when you accepted its " +
			"contact request with share_contact. No student verification is needed. A grant that does not share it " +
			"changes nothing; another user's grant is 404.",
	}, op.Meta{Action: string(authz.StudentsAccessGrantRevoke), Surface: op.SurfaceSelf}), h.unshareMyGrantContact)
}

type unshareMyGrantContactInput struct {
	GrantID string `path:"grant_id" doc:"Grant ID (another user's or a malformed ID is 404)"`
}

func (h handler) unshareMyGrantContact(ctx context.Context, in *unshareMyGrantContactInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(in.GrantID)
	if err != nil {
		id = uuid.Nil() // the role check decides first, then the unknown grant is 404
	}
	return nil, h.svc.UnshareMyAccessGrantContact(ctx, a, id)
}
