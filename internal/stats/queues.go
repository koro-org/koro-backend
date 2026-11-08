package stats

import (
	"context"
	"errors"

	"koro/internal/authz"
	"koro/internal/feed"
	"koro/internal/orgs"
)

// Queues implements Service (getAdminQueues; BE-196): the review queue
// sizes of the admin surface for every platform role (k37), each counter
// gated again by its queue's own list action (the rule of BE-192), so a
// role learns the size of the queues it works and nothing else:
//
//	org_reviews        orgs.org.list_admin              moderator, support, platform_admin
//	club_claims        directory.club_claim.list        support, platform_admin
//	content_reports    feed.report.list                 moderator, platform_admin
//	kvkk_requests      kvkk.dsr.list (kvkk's count)     dpo, platform_admin (k32)
//	student_documents  identity.edu_verification.list   support, platform_admin
//
// A queue the caller may not work is null and is not counted; a failing
// count fails the answer (no partial queue card). The figures are the
// platform panel's queue counters (getAdminStats), computed the same way.
func (s *service) Queues(ctx context.Context, a authz.Actor) (QueueStats, error) {
	if err := authz.Can(ctx, a, authz.StatsQueuesRead, adminSurface); err != nil {
		return QueueStats{}, err
	}
	var out QueueStats
	var err error
	if out.OrgReviews, err = s.staffQueue(ctx, a, authz.OrgsOrgListAdmin, s.orgReviews); err != nil {
		return QueueStats{}, err
	}
	if out.ClubClaims, err = s.staffQueue(ctx, a, authz.DirectoryClubClaimList, s.d.Universities.PendingClaims); err != nil {
		return QueueStats{}, err
	}
	if out.ContentReports, err = s.staffQueue(ctx, a, authz.FeedReportList, s.contentReports); err != nil {
		return QueueStats{}, err
	}
	if out.KvkkRequests, out.StudentDocuments, err = s.moduleQueues(ctx, a); err != nil {
		return QueueStats{}, err
	}
	return out, nil
}

// staffQueue is a queue counter over a module's system read (its
// Statistics read, which authorises nobody): stats gates it like the
// queue's own list does first (adminListOrgs, adminListClubClaims,
// listReports: the list action on the admin surface). The count of a
// caller refused is not read.
func (s *service) staffQueue(ctx context.Context, a authz.Actor, act authz.Action,
	count func(context.Context) (int, error)) (*int, error) {
	if err := authz.Can(ctx, a, act, adminSurface); err != nil {
		return queueSize(0, err)
	}
	return queueSize(count(ctx))
}

// moduleQueues are the counters the owning modules gate themselves with
// the queue's list action (BE-192): kvkk counts what kvkk.dsr.list would
// show the caller (03 k32), identity the pending student documents.
func (s *service) moduleQueues(ctx context.Context, a authz.Actor) (kvkk, docs *int, err error) {
	if kvkk, err = queueSize(s.d.DataRequests.PendingRequests(ctx, a)); err != nil {
		return nil, nil, err
	}
	if docs, err = queueSize(s.d.StudentDocs.PendingEduDocuments(ctx, a)); err != nil {
		return nil, nil, err
	}
	return kvkk, docs, nil
}

// queueSize maps a queue counter gated by the queue's own action: the
// count for a caller who may work the queue, nil for one the module refuses
// (403/404 of the authorisation), any other error as is (an MFA refusal
// included: it is not "this role does not work the queue").
func queueSize(n int, err error) (*int, error) {
	switch {
	case err == nil:
		return &n, nil
	case errors.Is(err, authz.ErrForbidden), errors.Is(err, authz.ErrNotFound):
		return nil, nil
	}
	return nil, err
}

// orgReviews counts the orgs awaiting verification. The range of the org
// counts is empty: only the counts by status are used.
func (s *service) orgReviews(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	st, err := s.d.OrgCounts.OrgStats(ctx, now, now)
	if err != nil {
		return 0, err
	}
	return pendingOrgReviews(st), nil
}

// contentReports counts the content reports open or in review. The range
// of the content counts is empty: only the reports by status are used.
func (s *service) contentReports(ctx context.Context) (int, error) {
	now := s.d.Clock.Now()
	st, err := s.d.Content.ContentStats(ctx, now, now)
	if err != nil {
		return 0, err
	}
	return openContentReports(st), nil
}

// pendingOrgReviews is the org review queue: orgs of any kind in
// pending_review.
func pendingOrgReviews(st orgs.OrgStats) int {
	n := 0
	for _, c := range st.Counts {
		if c.Status == orgs.StatusPendingReview {
			n += c.Count
		}
	}
	return n
}

// openContentReports is the content report queue: reports open or in
// review. It is the queue's size, not the caller's page: listReports
// leaves out the reports the caller has a stake in (BE-183, BE-194), the
// count does not, and names none of them.
func openContentReports(st feed.ContentStats) int {
	return st.ReportsByStatus[feed.ReportOpen] + st.ReportsByStatus[feed.ReportInReview]
}
