package stats

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/entitlements"
	"koro/internal/events"
	"koro/internal/feed"
	"koro/internal/messaging"
	"koro/internal/orgs"
	"koro/internal/outreach"
	"koro/internal/platform/problem"
	"koro/internal/students"
)

// ErrNotFound is the 404 of a missing org or a panel of another org kind.
var ErrNotFound = problem.ErrNotFound

// Limits of the lists in the answers.
const (
	maxClubEvents      = 50 // ClubEventsDTO.Items
	maxUniversityClubs = 50 // UniversityClubsDTO.Items
)

var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// Platform implements Service.
func (s *service) Platform(ctx context.Context, a authz.Actor, q RangeQuery) (PlatformStats, error) {
	if err := authz.Can(ctx, a, authz.StatsPlatformRead, adminSurface); err != nil {
		return PlatformStats{}, err
	}
	p, err := q.resolve(s.d.Clock.Now(), lastDays)
	if err != nil {
		return PlatformStats{}, err
	}
	from, to := p.bounds()
	users, err := s.d.Users.UserStats(ctx, from, to)
	if err != nil {
		return PlatformStats{}, err
	}
	org, err := s.d.OrgCounts.OrgStats(ctx, from, to)
	if err != nil {
		return PlatformStats{}, err
	}
	content, err := s.d.Content.ContentStats(ctx, from, to)
	if err != nil {
		return PlatformStats{}, err
	}
	ev, err := s.d.Events.EventStats(ctx, from, to)
	if err != nil {
		return PlatformStats{}, err
	}
	claims, err := s.d.Universities.PendingClaims(ctx)
	if err != nil {
		return PlatformStats{}, err
	}
	out := PlatformStats{Range: p.dto(),
		Users: PlatformUsersDTO{ByStatus: keyCounts(users.ByStatus), ByPersona: keyCounts(users.ByPersona),
			EmailVerified: users.EmailVerified, VerifiedStudents: users.VerifiedStudents, NewInRange: users.NewInRange},
		Orgs: platformOrgs(org),
		Content: PlatformContentDTO{PostsByStatus: keyCounts(content.PostsByStatus), PublishedInRange: content.PublishedInRange,
			ReportsByStatus: keyCounts(content.ReportsByStatus), ReportsInRange: content.ReportsInRange},
		Events: PlatformEventsDTO{Upcoming: ev.Upcoming, HeldInRange: ev.Held, CancelledInRange: ev.Cancelled,
			Registered: ev.Registered, CheckedIn: ev.CheckedIn, AttendanceRate: rate(ev.CheckedIn, ev.Registered)},
		Queues: PlatformQueuesDTO{ClubClaims: claims,
			ContentReports: content.ReportsByStatus[feed.ReportOpen] + content.ReportsByStatus[feed.ReportInReview]},
	}
	for _, n := range users.ByStatus {
		out.Users.Total += n
	}
	for _, c := range org.Counts {
		if c.Status == orgs.StatusPendingReview {
			out.Queues.OrgReviews += c.Count
		}
	}
	return out, nil
}

// platformOrgs groups the org counts by kind.
func platformOrgs(st orgs.OrgStats) PlatformOrgsDTO {
	out := PlatformOrgsDTO{ByKind: []OrgKindDTO{}}
	for _, c := range st.Counts { // by kind, then status
		if n := len(out.ByKind); n == 0 || out.ByKind[n-1].Kind != string(c.Kind) {
			out.ByKind = append(out.ByKind, OrgKindDTO{Kind: string(c.Kind), ByStatus: []KeyCount{},
				NewInRange: st.NewInRange[c.Kind]})
		}
		k := &out.ByKind[len(out.ByKind)-1]
		k.Total += c.Count
		k.ByStatus = append(k.ByStatus, KeyCount{Key: string(c.Status), Count: c.Count})
	}
	for _, n := range st.NewInRange {
		out.NewInRange += n
	}
	return out
}

// Club implements Service.
func (s *service) Club(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RangeQuery) (ClubStats, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	if err != nil {
		return ClubStats{}, err
	}
	sensitive, err := s.sensitive(ctx, o)
	if err != nil {
		return ClubStats{}, err
	}
	res := authz.OrgResource(o.ID, string(o.Kind), string(o.Status))
	res.Attrs[authz.AttrSensitive] = strconv.FormatBool(sensitive)
	if err := authz.Can(ctx, a, authz.StatsClubRead, res); err != nil {
		return ClubStats{}, err
	}
	if o.Kind != orgs.KindClub {
		return ClubStats{}, ErrNotFound
	}
	p, err := q.resolve(s.d.Clock.Now(), lastMonths)
	if err != nil {
		return ClubStats{}, err
	}
	from, to := p.bounds()
	members, err := s.d.OrgCounts.MemberStats(ctx, o.ID, from, to)
	if err != nil {
		return ClubStats{}, err
	}
	figures, err := s.d.Events.OrgEventFigures(ctx, []uuid.UUID{o.ID}, from, to)
	if err != nil {
		return ClubStats{}, err
	}
	sales, err := s.d.Sales.SalesStats(ctx, o.ID, from, to)
	if err != nil {
		return ClubStats{}, err
	}
	return ClubStats{Range: p.dto(), Members: clubMembers(members, p), Events: s.clubEvents(figures),
		Sponsorship: ClubSponsorshipDTO{Orders: sales.Orders, ByStatus: keyCounts(sales.ByStatus)}}, nil
}

// sensitive reports whether o is a sensitive club. A club without its
// catalog row is treated as sensitive (fail closed: staff do not see it).
func (s *service) sensitive(ctx context.Context, o orgs.OrgSummary) (bool, error) {
	if o.Kind != orgs.KindClub {
		return false, nil
	}
	c, err := s.d.Clubs.GetClub(ctx, o.ID)
	switch {
	case errors.Is(err, directory.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return c.IsSensitive, nil
}

// clubMembers fills every month of the period.
func clubMembers(st orgs.MemberStats, p period) ClubMembersDTO {
	byMonth := make(map[string]orgs.MemberMonth, len(st.Monthly))
	for _, m := range st.Monthly {
		byMonth[m.Month] = m
	}
	out := ClubMembersDTO{Active: st.Active, PendingRequests: st.Pending, JoinedInRange: st.JoinedInRange,
		LeftInRange: st.LeftInRange, NetGrowth: st.JoinedInRange - st.LeftInRange, Monthly: []MonthDTO{}}
	for _, m := range p.months() {
		out.Monthly = append(out.Monthly, MonthDTO{Month: m, Joined: byMonth[m].Joined, Left: byMonth[m].Left})
	}
	return out
}

// clubEvents sums the club's events; the rates cover the events held by
// now (started, not cancelled).
func (s *service) clubEvents(figures []events.EventFigures) ClubEventsDTO {
	now := s.d.Clock.Now()
	out := ClubEventsDTO{Items: []ClubEventDTO{}, Partial: len(figures) >= events.MaxOrgEventFigures}
	for _, f := range figures {
		if f.Status == events.StatusCancelled {
			out.Cancelled++
		} else {
			out.Count++
		}
		held := f.Status != events.StatusCancelled && !f.StartsAt.After(now)
		item := ClubEventDTO{EventID: f.EventID.String(), Title: f.Title, Status: string(f.Status), StartsAt: f.StartsAt,
			Held: held, Registered: f.Registered, CheckedIn: f.CheckedIn, NoShow: f.NoShow}
		if held {
			out.Registered += f.Registered
			out.CheckedIn += f.CheckedIn
			out.NoShow += f.NoShow
			item.AttendanceRate = rate(f.CheckedIn, f.Registered)
		}
		if len(out.Items) < maxClubEvents {
			out.Items = append(out.Items, item)
		}
	}
	out.AttendanceRate = rate(out.CheckedIn, out.Registered)
	return out
}

// Company implements Service.
func (s *service) Company(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RangeQuery) (CompanyStats, error) {
	o, err := s.orgOf(ctx, a, authz.StatsCompanyRead, orgID, orgs.KindCompany)
	if err != nil {
		return CompanyStats{}, err
	}
	p, err := q.resolve(s.d.Clock.Now(), lastDays)
	if err != nil {
		return CompanyStats{}, err
	}
	from, to := p.bounds()
	reach, err := s.d.Outreach.OrgOutreachStats(ctx, o.ID, from, to)
	if err != nil {
		return CompanyStats{}, err
	}
	posts, err := s.d.Content.OrgPostStats(ctx, o.ID, from, to)
	if err != nil {
		return CompanyStats{}, err
	}
	contacts, err := s.d.Contacts.ContactRequestStats(ctx, o.ID, from, to)
	if err != nil {
		return CompanyStats{}, err
	}
	usage, err := s.d.Packages.Usage(ctx, o.ID)
	if err != nil {
		return CompanyStats{}, err
	}
	out := CompanyStats{Range: p.dto(),
		Audience: CompanyAudienceDTO{ActiveSegments: reach.ActiveSegments, Segments: make([]SegmentEstimateDTO, 0, len(reach.Segments))},
		Posts:    CompanyPostsDTO{Published: posts.Published, Targeted: posts.Targeted, Likes: posts.Likes, Saves: posts.Saves},
		Dispatches: CompanyDispatchesDTO{Count: reach.Dispatches, ByStatus: keyCounts(reach.ByStatus),
			Eligible: estimate(reach.Eligible), Sent: estimate(reach.Sent)},
		ContactRequests: CompanyContactsDTO{Sent: contacts.Sent, ByStatus: keyCounts(contacts.ByStatus)},
		Package:         make([]FeatureUsageDTO, len(usage)),
	}
	for _, seg := range reach.Segments {
		out.Audience.Segments = append(out.Audience.Segments, SegmentEstimateDTO{SegmentID: seg.ID.String(), Name: seg.Name,
			Estimate: estimate(seg.Estimate), CountedAt: seg.CountedAt})
	}
	accepted := contacts.ByStatus[messaging.StatusAccepted]
	answered := accepted + contacts.ByStatus[messaging.StatusDeclined] + contacts.ByStatus[messaging.StatusExpired]
	out.ContactRequests.AcceptanceRate = rate(accepted, answered)
	for i, u := range usage {
		out.Package[i] = featureUsage(u)
	}
	return out, nil
}

func featureUsage(u entitlements.FeatureUsage) FeatureUsageDTO {
	return FeatureUsageDTO{Feature: string(u.Feature), Total: u.Total, Used: u.Used, Remaining: u.Remaining, AccessUntil: u.AccessUntil}
}

// University implements Service.
func (s *service) University(ctx context.Context, a authz.Actor, orgID uuid.UUID, q RangeQuery) (UniversityStats, error) {
	o, err := s.orgOf(ctx, a, authz.StatsUniversityRead, orgID, orgs.KindUniversity)
	if err != nil {
		return UniversityStats{}, err
	}
	p, err := q.resolve(s.d.Clock.Now(), lastMonths)
	if err != nil {
		return UniversityStats{}, err
	}
	from, to := p.bounds()
	uc, err := s.d.Universities.UniversityClubStats(ctx, o.ID)
	switch {
	case errors.Is(err, directory.ErrNotFound): // a university org without its directory row: nothing to count
		uc = directory.UniversityClubs{}
	case err != nil:
		return UniversityStats{}, err
	}
	out := UniversityStats{Range: p.dto(), Pending: UniversityPendingDTO{ClubClaims: uc.PendingClaims}}
	if uc.UniversityID != uuid.Nil() {
		st, err := s.d.Students.UniversityStudentStats(ctx, uc.UniversityID, from, to)
		if err != nil {
			return UniversityStats{}, err
		}
		out.Students = universityStudents(st)
	} else {
		out.Students = universityStudents(students.UniversityStudents{})
	}
	all, open := make([]uuid.UUID, 0, len(uc.Clubs)), make([]uuid.UUID, 0, len(uc.Clubs))
	for _, c := range uc.Clubs {
		all = append(all, c.OrgID)
		if !c.Sensitive {
			open = append(open, c.OrgID)
		}
		if c.Claimed {
			out.Clubs.Claimed++
		}
	}
	out.Clubs.Total = len(uc.Clubs)
	if out.Pending.OfferingApprovals, err = s.d.Offerings.PendingUniversityDecisions(ctx, all); err != nil {
		return UniversityStats{}, err
	}
	totals, err := s.d.Events.OrgEventTotals(ctx, open, from, to)
	if err != nil {
		return UniversityStats{}, err
	}
	if out.Clubs, err = s.clubActivity(ctx, out.Clubs, totals); err != nil {
		return UniversityStats{}, err
	}
	return out, nil
}

// universityStudents rounds the suppressed counts of students to buckets.
func universityStudents(st students.UniversityStudents) UniversityStudentsDTO {
	return UniversityStudentsDTO{Total: estimateOf(st.Total), NewInRange: estimateOf(st.NewInRange),
		ByDegreeLevel: groupEstimates(st.ByDegreeLevel), ByClassYear: groupEstimates(st.ByClassYear),
		ByDepartment: groupEstimates(st.ByDepartment)}
}

// clubActivity fills the activity figures of the non-sensitive clubs.
func (s *service) clubActivity(ctx context.Context, out UniversityClubsDTO, totals map[uuid.UUID]events.EventTotals) (UniversityClubsDTO, error) {
	out.Items = []ClubActivityDTO{}
	ids := make([]uuid.UUID, 0, len(totals))
	participants := 0
	for id, t := range totals {
		ids = append(ids, id)
		out.EventsInRange += t.Events
		participants += t.Registered
	}
	out.ActiveInRange = len(ids)
	out.Participants = estimate(outreach.PublicEstimate(participants))
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		return cmp.Or(cmp.Compare(totals[b].Events, totals[a].Events), cmp.Compare(a.String(), b.String()))
	})
	if len(ids) > maxUniversityClubs {
		ids = ids[:maxUniversityClubs]
	}
	names, err := s.d.Orgs.GetMany(ctx, ids)
	if err != nil {
		return UniversityClubsDTO{}, err
	}
	for _, id := range ids {
		out.Items = append(out.Items, ClubActivityDTO{OrgID: id.String(), Name: names[id].Name, Events: totals[id].Events,
			Participants: estimate(outreach.PublicEstimate(totals[id].Registered))})
	}
	return out, nil
}

// orgOf reads the path's org, authorises act on it and keeps the panel to
// its org kind (404 otherwise, after the authorisation).
func (s *service) orgOf(ctx context.Context, a authz.Actor, act authz.Action, orgID uuid.UUID, kind orgs.Kind) (orgs.OrgSummary, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	if err != nil {
		return orgs.OrgSummary{}, err
	}
	if err := authz.Can(ctx, a, act, authz.OrgResource(o.ID, string(o.Kind), string(o.Status))); err != nil {
		return orgs.OrgSummary{}, err
	}
	if o.Kind != kind {
		return orgs.OrgSummary{}, ErrNotFound
	}
	return o, nil
}

// keyCounts turns a count map into rows by key.
func keyCounts[K ~string](m map[K]int) []KeyCount {
	out := make([]KeyCount, 0, len(m))
	for k, n := range m {
		out = append(out, KeyCount{Key: string(k), Count: n})
	}
	slices.SortFunc(out, func(a, b KeyCount) int { return cmp.Compare(a.Key, b.Key) })
	return out
}

// estimate is the answer form of an outreach estimate.
func estimate(e outreach.Estimate) Estimate {
	return Estimate{Count: e.Count, BelowThreshold: e.BelowThreshold}
}

// estimateOf publishes a count students suppressed below 10 (nil).
func estimateOf(n *int) Estimate {
	if n == nil {
		return Estimate{BelowThreshold: true}
	}
	return estimate(outreach.PublicEstimate(*n))
}

// groupEstimates rounds the groups (each at least 10) to their buckets.
func groupEstimates(gs []students.StudentGroup) []KeyEstimate {
	out := make([]KeyEstimate, 0, len(gs))
	for _, g := range gs {
		out = append(out, KeyEstimate{Key: g.Key, Count: outreach.Bucket(g.Count)})
	}
	return out
}

// rate is part/whole rounded to three decimals; nil for an empty whole.
func rate(part, whole int) *float64 {
	if whole <= 0 {
		return nil
	}
	r := math.Round(float64(part)/float64(whole)*1000) / 1000
	return &r
}
