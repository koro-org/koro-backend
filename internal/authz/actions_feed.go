// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the feed module (03 §5.7), in table order.
const (
	FeedFeedRead             Action = "feed.feed.read"
	FeedPostRead             Action = "feed.post.read"
	FeedPostListByOrg        Action = "feed.post.list_by_org"
	FeedPostListOrgManage    Action = "feed.post.list_org_manage"
	FeedPostCreate           Action = "feed.post.create"
	FeedPostCreateTargeted   Action = "feed.post.create_targeted"
	FeedPostUpdate           Action = "feed.post.update"
	FeedPostPublish          Action = "feed.post.publish"
	FeedPostDelete           Action = "feed.post.delete"
	FeedPostStats            Action = "feed.post.stats"
	FeedReactionManage       Action = "feed.reaction.manage"
	FeedSaveManage           Action = "feed.save.manage"
	FeedSaveListOwn          Action = "feed.save.list_own"
	FeedHideCreate           Action = "feed.hide.create"
	FeedHideDelete           Action = "feed.hide.delete"
	FeedHideListOwn          Action = "feed.hide.list_own"
	FeedFollowManage         Action = "feed.follow.manage"
	FeedFollowListOwn        Action = "feed.follow.list_own"
	FeedMuteManage           Action = "feed.mute.manage"
	FeedMuteListOwn          Action = "feed.mute.list_own"
	FeedReportCreate         Action = "feed.report.create"
	FeedReportList           Action = "feed.report.list"
	FeedReportRead           Action = "feed.report.read"
	FeedReportResolve        Action = "feed.report.resolve"
	FeedReviewQueueList      Action = "feed.review_queue.list"
	FeedPostModerate         Action = "feed.post.moderate"
	FeedOrgModerate          Action = "feed.org.moderate"
	FeedModerationActionList Action = "feed.moderation_action.list"
)

// System-only actions of the feed module (03 §3.7): only SystemActor may perform them.
const (
	FeedOrgSettingsAutoTrust Action = "feed.org_settings.auto_trust"
)

// feedActions lists the HTTP actions of feed in 03 §5.7 order.
var feedActions = []Action{
	FeedFeedRead,
	FeedPostRead,
	FeedPostListByOrg,
	FeedPostListOrgManage,
	FeedPostCreate,
	FeedPostCreateTargeted,
	FeedPostUpdate,
	FeedPostPublish,
	FeedPostDelete,
	FeedPostStats,
	FeedReactionManage,
	FeedSaveManage,
	FeedSaveListOwn,
	FeedHideCreate,
	FeedHideDelete,
	FeedHideListOwn,
	FeedFollowManage,
	FeedFollowListOwn,
	FeedMuteManage,
	FeedMuteListOwn,
	FeedReportCreate,
	FeedReportList,
	FeedReportRead,
	FeedReportResolve,
	FeedReviewQueueList,
	FeedPostModerate,
	FeedOrgModerate,
	FeedModerationActionList,
}
