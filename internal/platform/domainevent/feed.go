package domainevent

import "uuid"

// Topics of the feed module (01 §6.2). Owner: feed tickets.
const (
	TopicPostSubmittedForReview = "post.submitted_for_review"
	TopicPostPublished          = "post.published"
	TopicPostModerated          = "post.moderated"
	TopicPostLiked              = "post.liked"
	TopicPostUnliked            = "post.unliked"
	TopicPostSaved              = "post.saved"
	TopicContentReportCreated   = "content_report.created"
)

var _ = register(
	spec("feed", AfterCommit, PostSubmittedForReview{}),
	spec("feed", AfterCommit, PostPublished{}),
	spec("feed", AfterCommit, PostModerated{}),
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
