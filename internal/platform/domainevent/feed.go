package domainevent

import "uuid"

// Topics of the feed module (01 §6.2). Owner: feed tickets.
const (
	TopicPostSubmittedForReview = "post.submitted_for_review"
	TopicPostPublished          = "post.published"
	TopicPostModerated          = "post.moderated"
	TopicPostChanged            = "post.changed"
	TopicPostLiked              = "post.liked"
	TopicPostUnliked            = "post.unliked"
	TopicPostSaved              = "post.saved"
	TopicContentReportCreated   = "content_report.created"
)

var _ = register(
	spec("feed", AfterCommit, PostSubmittedForReview{}),
	spec("feed", AfterCommit, PostPublished{}),
	spec("feed", AfterCommit, PostModerated{}),
	spec("feed", Outbox, PostChanged{}),
	spec("feed", AfterCommit, PostLiked{}),
	spec("feed", AfterCommit, PostUnliked{}),
	spec("feed", AfterCommit, PostSaved{}),
	spec("feed", AfterCommit, ContentReportCreated{}),
)

// PostSubmittedForReview: moderators get an in-app notification.
type PostSubmittedForReview struct {
	PostID uuid.UUID `json:"post_id"`
	OrgID  uuid.UUID `json:"org_id"`
}

// Topic implements Payload.
func (PostSubmittedForReview) Topic() string { return TopicPostSubmittedForReview }

// PostPublished: followers with notify=true get a push digest
// (commercial posts excluded).
type PostPublished struct {
	PostID     uuid.UUID `json:"post_id"`
	OrgID      uuid.UUID `json:"org_id"`
	Kind       string    `json:"kind"`
	Commercial bool      `json:"commercial"`
}

// Topic implements Payload.
func (PostPublished) Topic() string { return TopicPostPublished }

// PostModerated: the author org admins are notified.
type PostModerated struct {
	PostID     uuid.UUID `json:"post_id"`
	OrgID      uuid.UUID `json:"org_id"`
	Action     string    `json:"action"`
	ReasonCode string    `json:"reason_code"`
}

// Topic implements Payload.
func (PostModerated) Topic() string { return TopicPostModerated }

// PostChanged: what readers see of a post changed (BE-149): it was
// published (or approved), edited while published, hidden, removed or
// restored. Written in the transaction (O): the web keeps its post pages
// cached until told (webcache). Public reports whether an anonymous reader
// opens it when published (audience public, not commercial; T-57), i.e.
// whether it is on the public web; kind, audience and label do not change
// after publication, so it holds before and after the change. Change is
// one of published, edited, hidden, removed, restored.
type PostChanged struct {
	PostID uuid.UUID `json:"post_id"`
	OrgID  uuid.UUID `json:"org_id"`
	Public bool      `json:"public"`
	Change string    `json:"change"`
}

// Topic implements Payload.
func (PostChanged) Topic() string { return TopicPostChanged }

// PostLiked: talent signal feed_like.
type PostLiked struct {
	PostID      uuid.UUID `json:"post_id"`
	UserID      uuid.UUID `json:"user_id"`
	AuthorOrgID uuid.UUID `json:"author_org_id"`
}

// Topic implements Payload.
func (PostLiked) Topic() string { return TopicPostLiked }

// PostUnliked: reverses PostLiked.
type PostUnliked struct {
	PostID      uuid.UUID `json:"post_id"`
	UserID      uuid.UUID `json:"user_id"`
	AuthorOrgID uuid.UUID `json:"author_org_id"`
}

// Topic implements Payload.
func (PostUnliked) Topic() string { return TopicPostUnliked }

// PostSaved: talent signal feed_save.
type PostSaved struct {
	PostID      uuid.UUID `json:"post_id"`
	UserID      uuid.UUID `json:"user_id"`
	AuthorOrgID uuid.UUID `json:"author_org_id"`
}

// Topic implements Payload.
func (PostSaved) Topic() string { return TopicPostSaved }

// ContentReportCreated: moderators get an in-app notification.
type ContentReportCreated struct {
	ReportID   uuid.UUID `json:"report_id"`
	TargetType string    `json:"target_type"`
}

// Topic implements Payload.
func (ContentReportCreated) Topic() string { return TopicContentReportCreated }
