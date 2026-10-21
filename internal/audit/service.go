package audit

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Range limits of the HTTP use cases.
const (
	maxListRange   = 366 * 24 * time.Hour // occurred_from … occurred_to
	maxVerifyDays  = 31                   // from_day … to_day, inclusive
	talentPrefix   = "talent."            // 03 §5.21: only the existence of talent.* events is shown
	ipv4PrefixBits = 24
	ipv6PrefixBits = 48
)

// Log implements Logger.
func (s *service) Log(ctx context.Context, e Event) error {
	r, err := s.record(ctx, e)
	if err != nil {
		return err
	}
	return s.d.Store.Insert(ctx, r)
}

// ListOrg implements Service.
func (s *service) ListOrg(ctx context.Context, a authz.Actor, orgID uuid.UUID, q OrgQuery) (Page[View], error) {
	if err := authz.Can(ctx, a, authz.AuditEventListOrg, authz.Resource{Type: authz.TypeAuditLog, ID: orgID, OrgID: &orgID}); err != nil {
		return Page[View]{}, err
	}
	if err := checkRange(q.From, q.To); err != nil {
		return Page[View]{}, err
	}
	f := Filter{
		From: q.From, To: q.To, Action: q.Action, ActorUserID: q.ActorUserID,
		// Sensitive club records go to that club's club_admin only (03 §5.21).
		HideSensitive: !a.HasOrgRole(orgID, authz.MemberClubAdmin) || a.IsMinor,
	}
	limit := page.ClampLimit(q.Limit)
	rows, err := s.d.Store.ListOrg(ctx, orgID, f, q.After, limit+1)
	if err != nil {
		return Page[View]{}, err
	}
	rows, more := page.Cut(rows, limit)
	// IP and User-Agent only for dpo and platform_admin (04 §2.21).
	network := a.HasPlatformRole(authz.PlatformDPO) || a.HasPlatformRole(authz.PlatformAdmin)
	items, err := s.views(ctx, rows, network, false)
	if err != nil {
		return Page[View]{}, err
	}
	return Page[View]{Items: items, Next: next(rows, more)}, nil
}

// AdminList implements Service. The search is written to the log in the
// same transaction as the read (03 §6.5: who searched whose records).
func (s *service) AdminList(ctx context.Context, a authz.Actor, q AdminQuery) (Page[View], error) {
	if err := authz.Can(ctx, a, authz.AuditEventList, authz.Resource{Type: authz.TypeAdminSurface}); err != nil {
		return Page[View]{}, err
	}
	if err := checkRange(q.From, q.To); err != nil {
		return Page[View]{}, err
	}
	f := Filter{
		From: q.From, To: q.To, ActorUserID: q.ActorUserID, OrgID: q.OrgID, ResourceType: q.ResourceType,
		ResourceID: q.ResourceID, Action: q.Action, Outcome: q.Outcome, BreakGlass: q.BreakGlass,
	}
	limit := page.ClampLimit(q.Limit)
	var rows []Record
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		ev := Event{Action: string(authz.AuditEventList), Outcome: Success, ResourceType: authz.TypeAuditLog,
			Metadata: searchMeta(q)}.By(a)
		if err := s.Log(ctx, ev); err != nil {
			return err
		}
		var err error
		rows, err = s.d.Store.List(ctx, f, q.After, limit+1)
		return err
	})
	if err != nil {
		return Page[View]{}, err
	}
	rows, more := page.Cut(rows, limit)
	items, err := s.views(ctx, rows, true, true)
	if err != nil {
		return Page[View]{}, err
	}
	return Page[View]{Items: items, Next: next(rows, more)}, nil
}

// Verify implements Service.
func (s *service) Verify(ctx context.Context, a authz.Actor, from, to civil.Date) (VerifyResult, error) {
	if err := authz.Can(ctx, a, authz.AuditChainVerify, authz.Resource{Type: authz.TypeAdminSurface}); err != nil {
		return VerifyResult{}, err
	}
	today := civil.Today(s.now())
	switch {
	case !from.IsValid() || !to.IsValid():
		return VerifyResult{}, rangeErr("body.from_day", "from_day and to_day are required dates")
	case to.Before(from):
		return VerifyResult{}, rangeErr("body.to_day", "to_day is before from_day")
	case to.After(today):
		return VerifyResult{}, rangeErr("body.to_day", "to_day is in the future")
	case from.AddDays(maxVerifyDays - 1).Before(to):
		return VerifyResult{}, rangeErr("body.to_day", fmt.Sprintf("at most %d days per request", maxVerifyDays))
	}
	res, err := s.verifyRange(ctx, from, to)
	if err != nil {
		return VerifyResult{}, err
	}
	if !res.OK {
		s.alarm(ctx, Alarm{Kind: AlarmChainBroken, FirstBadChainSeq: res.FirstBadChainSeq, Detail: res.Problem})
	}
	outcome := Success
	if !res.OK {
		outcome = Failure
	}
	record := func(ctx context.Context) (VerifyResult, error) {
		ev := Event{Action: string(authz.AuditChainVerify), Outcome: outcome, Metadata: verifyMeta(from, to, res)}.By(a)
		return res, s.Log(ctx, ev)
	}
	if s.d.Idempotency == nil {
		var out VerifyResult
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			var err error
			out, err = record(ctx)
			return err
		})
		return out, err
	}
	hash, err := idempotency.Hash(struct{ From, To string }{from.String(), to.String()})
	if err != nil {
		return VerifyResult{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, record)
	return out, err
}

// views turns rows into what the reader may see.
func (s *service) views(ctx context.Context, rows []Record, network, metadata bool) ([]View, error) {
	names := map[uuid.UUID]string{}
	if s.d.ActorNames != nil && len(rows) > 0 {
		var ids []uuid.UUID
		for _, r := range rows {
			if r.ActorUserID != nil && !slices.Contains(ids, *r.ActorUserID) {
				ids = append(ids, *r.ActorUserID)
			}
		}
		var err error
		if names, err = s.d.ActorNames.DisplayNames(ctx, ids); err != nil {
			return nil, err
		}
	}
	out := make([]View, len(rows))
	for i, r := range rows {
		v := View{ID: r.ID, Action: r.Action, Outcome: r.Outcome, ActorUserID: r.ActorUserID,
			ResourceType: r.ResourceType, ResourceID: r.ResourceID, Reason: r.Reason, OccurredAt: r.OccurredAt}
		if r.ActorUserID != nil {
			v.ActorDisplay = names[*r.ActorUserID]
		}
		if network {
			v.IP, v.UserAgent = r.IP, r.UserAgent
		}
		if metadata {
			v.Metadata = r.Metadata
		}
		if strings.HasPrefix(r.Action, talentPrefix) { // existence only: actor, time, reason (03 §5.21)
			v.ResourceType, v.ResourceID, v.IP, v.UserAgent, v.Metadata = "", nil, "", "", nil
		}
		out[i] = v
	}
	return out, nil
}

func next(rows []Record, more bool) *After {
	if !more || len(rows) == 0 {
		return nil
	}
	last := rows[len(rows)-1]
	return &After{At: last.OccurredAt, ID: last.ID}
}

func rangeErr(location, msg string) error {
	return ErrInvalidRange.WithFields(problem.FieldError{Location: location, Message: msg})
}

// checkRange validates a list's [from, to) window.
func checkRange(from, to time.Time) error {
	switch {
	case from.IsZero() || to.IsZero():
		return rangeErr("query.occurred_from", "occurred_from and occurred_to are required")
	case !from.Before(to):
		return rangeErr("query.occurred_to", "occurred_to must be after occurred_from")
	case to.Sub(from) > maxListRange+24*time.Hour: // a date-only "to" is the end of that day
		return rangeErr("query.occurred_to", "the range spans more than 366 days")
	}
	return nil
}

// searchMeta records the filters of an admin search: IDs, kinds and the
// window, never free text beyond the action name.
func searchMeta(q AdminQuery) map[string]any {
	m := map[string]any{"occurred_from": q.From.UTC().Format(time.RFC3339), "occurred_to": q.To.UTC().Format(time.RFC3339)}
	put := func(k string, v *uuid.UUID) {
		if v != nil {
			m[k] = v.String()
		}
	}
	put("filter_actor_user_id", q.ActorUserID)
	put("filter_org_id", q.OrgID)
	put("filter_resource_id", q.ResourceID)
	if q.ResourceType != "" {
		m["filter_resource_type"] = q.ResourceType
	}
	if q.Action != "" {
		m["filter_action"] = q.Action
	}
	if q.Outcome != "" {
		m["filter_outcome"] = string(q.Outcome)
	}
	if q.BreakGlass != nil {
		m["filter_break_glass"] = *q.BreakGlass
	}
	if q.After != nil {
		m["next_page"] = true
	}
	return m
}

// ipPrefix returns the /24 (IPv4) or /48 (IPv6) network of ip.
func ipPrefix(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ""
	}
	bits := ipv6PrefixBits
	if a.Is4() {
		bits = ipv4PrefixBits
	}
	p, err := a.Prefix(bits)
	if err != nil {
		return ""
	}
	return p.String()
}

// uaFamilies maps User-Agent markers to coarse families, most specific first.
var uaFamilies = []struct{ marker, family string }{
	{"Koro/", "Koro app"}, {"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"},
	{"Chrome/", "Chrome"}, {"Safari/", "Safari"}, {"okhttp", "Android app"}, {"CFNetwork", "iOS app"},
	{"curl/", "curl"},
}

// userAgentFamily reduces a User-Agent to its family (no version, no device).
func userAgentFamily(ua string) string {
	if ua == "" {
		return ""
	}
	for _, f := range uaFamilies {
		if strings.Contains(ua, f.marker) {
			return f.family
		}
	}
	return "Other"
}
