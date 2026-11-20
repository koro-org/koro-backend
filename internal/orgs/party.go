package orgs

import (
	"context"
	"errors"
	"maps"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
)

// The party rule of an org's own surface (BE-214, Q-69; T-438): on the
// management surface of an org they belong to (any role; the platform org
// is no party's), platform staff act with the membership only: the authz
// policy gives them no staff path there (authz asMember) and the services
// lift the suspended org's ban, and skip the tax identifier review, only
// for platform admins who are no party (authz.ActsAsStaff). A write the
// rule refuses (403 forbidden, no new code) is recorded as denied with the
// reason of the staff decisions' rule (org_member), in its own transaction
// after the operation's rolled back:
//
//	updateOrg, closeOrg, transferOrgOwnership       the org
//	removeOrgMember, changeOrgMemberRole            the membership
//	createOrgInvitation, revokeOrgInvitation        the org, the invitation
//
// Refused reads (member lists, invitations) are not recorded: as every
// other refused read, the denial log and metric keep them (06 H-28).

// partyRefusal is a refusal of the party rule carried out of the operation
// (its transaction rolls back) with its denied record.
type partyRefusal struct {
	err    error
	denied audit.Event
}

func (p *partyRefusal) Error() string { return p.err.Error() }
func (p *partyRefusal) Unwrap() error { return p.err }

// partyDenied is the denied record of a party rule's refusal of act on r: the
// reason org_member, the org's scope (r's org was read from its row, 06
// H-38), r as the resource (the org when r names none; a sensitive club's
// record is marked), meta naming the refused operation, never business data.
func partyDenied(a authz.Actor, act authz.Action, r authz.Resource, meta map[string]any) audit.Event {
	org := *r.OrgID
	typ, id := r.Type, r.ID
	if id == uuid.Nil() {
		typ, id = resourceOrg, org
	}
	if r.Flag(authz.AttrSensitive) {
		meta = maps.Clone(meta)
		if meta == nil {
			meta = map[string]any{}
		}
		meta[audit.MetaSensitive] = true
	}
	return audit.Event{
		Action: string(act), Outcome: audit.Denied, OrgID: &org, ResourceType: typ, ResourceID: &id,
		Reason: reasonOrgMember, Metadata: meta,
	}.By(a)
}

// can authorises act on r, a resource of an org's own surface. A refusal of
// the party rule (authz.PartyRefused) comes back as a *partyRefusal that
// recordRefusal writes down; any other result as authz.Can returns it.
func can(ctx context.Context, a authz.Actor, act authz.Action, r authz.Resource, meta map[string]any) error {
	err := authz.Can(ctx, a, act, r)
	if err == nil || !authz.PartyRefused(ctx, a, act, r, err) {
		return err
	}
	return &partyRefusal{err: err, denied: partyDenied(a, act, r, meta)}
}

// recordRefusal writes the denied record of a party rule's refusal in err in
// its own transaction (the refusal must stay on record) and returns the
// refusal itself; any other err comes back as it is.
func (s *service) recordRefusal(ctx context.Context, err error) error {
	var p *partyRefusal
	if !errors.As(err, &p) {
		return err
	}
	if lerr := s.d.Tx.WithTx(ctx, func(ctx context.Context) error { return s.d.Audit.Log(ctx, p.denied) }); lerr != nil {
		return lerr
	}
	return p.err
}
