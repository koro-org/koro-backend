package feed

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// Kind is posts.kind (02 §12.2).
type Kind string

// Post kinds.
const (
	KindText            Kind = "text"             // Tiptap body, no media
	KindImage           Kind = "image"            // 1–10 images, optional body
	KindEventShare      Kind = "event_share"      // a published public event, optional body
	KindOpportunityCall Kind = "opportunity_call" // an opportunity (R-003 §1.6) with a body
)

// Valid reports whether k is a kind of the CHECK constraint.
func (k Kind) Valid() bool {
	return k == KindText || k == KindImage || k == KindEventShare || k == KindOpportunityCall
}

// Status is posts.status (02 §12.2).
type Status string

// Post statuses. A draft is published (trusted org) or goes to
// pending_review (new or restricted org; BE-032 approves); a moderator
// hides or removes (BE-032); the author org removes a published post.
const (
	StatusDraft         Status = "draft"
	StatusPendingReview Status = "pending_review"
	StatusPublished     Status = "published"
	StatusHidden        Status = "hidden"
	StatusRemoved       Status = "removed"
)

// Valid reports whether s is a status of the CHECK constraint.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusPendingReview, StatusPublished, StatusHidden, StatusRemoved:
		return true
	}
	return false
}

// AudienceKind is posts.audience_kind (02 §12.2).
type AudienceKind string

// Audience kinds. public and followers carry the keys ['all'] (posts_audience_all_chk);
// segment (targeted posts) is BE-032's.
const (
	AudiencePublic    AudienceKind = "public"
	AudienceFollowers AudienceKind = "followers"
	AudienceSegment   AudienceKind = "segment"
)

// Valid reports whether k is an audience kind of the CHECK constraint.
func (k AudienceKind) Valid() bool {
	return k == AudiencePublic || k == AudienceFollowers || k == AudienceSegment
}

// AuthorKind is posts.author_org_kind: the kind of the author org.
type AuthorKind string

// Author org kinds (posts_author_org_kind_chk; ngo is phase 2).
const (
	AuthorPlatform   AuthorKind = "platform"
	AuthorUniversity AuthorKind = "university"
	AuthorClub       AuthorKind = "club"
	AuthorCompany    AuthorKind = "company"
)

// Valid reports whether k is an author kind of the CHECK constraint.
func (k AuthorKind) Valid() bool {
	return k == AuthorPlatform || k == AuthorUniversity || k == AuthorClub || k == AuthorCompany
}

// KeyAll is the audience key every reader holds (02 §9.1).
const KeyAll = "all"

// Post is one posts row (02 §12.2; search_vector is never read).
type Post struct {
	ID                   uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	OrgID                uuid.UUID    `gorm:"type:uuid;column:org_id"`
	AuthorOrgKind        AuthorKind   `gorm:"column:author_org_kind"`
	AuthorUserID         uuid.UUID    `gorm:"type:uuid;column:author_user_id"`
	Kind                 Kind         `gorm:"column:kind"`
	Title                *string      `gorm:"column:title"`
	BodyJSON             JSON         `gorm:"column:body_json;type:jsonb"`
	BodyText             *string      `gorm:"column:body_text"`
	SchemaVersion        int16        `gorm:"column:schema_version"`
	EventID              *uuid.UUID   `gorm:"type:uuid;column:event_id"`
	Opportunity          JSON         `gorm:"column:opportunity;type:jsonb"`
	AudienceKind         AudienceKind `gorm:"column:audience_kind"`
	AudienceKeys         db.TextArray `gorm:"column:audience_keys;type:text[]"`
	AudienceDimCount     int16        `gorm:"column:audience_dim_count"`
	AudienceSnapshot     JSON         `gorm:"column:audience_snapshot;type:jsonb"`
	AudienceSizeEstimate *int         `gorm:"column:audience_size_estimate"`
	CommercialFlag       bool         `gorm:"column:commercial_flag"`
	Status               Status       `gorm:"column:status"`
	PublishedAt          *time.Time   `gorm:"column:published_at"`
	EditedAt             *time.Time   `gorm:"column:edited_at"`
	ReviewedBy           *uuid.UUID   `gorm:"type:uuid;column:reviewed_by"`
	ReviewedAt           *time.Time   `gorm:"column:reviewed_at"`
	RemovedAt            *time.Time   `gorm:"column:removed_at"`
	LikeCount            int          `gorm:"column:like_count"`
	SaveCount            int          `gorm:"column:save_count"`
	EntitlementUsageID   *uuid.UUID   `gorm:"type:uuid;column:entitlement_usage_id"`
	CreatedAt            time.Time    `gorm:"column:created_at"`
	UpdatedAt            time.Time    `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (Post) TableName() string { return "posts" }

func (p Post) clone() Post {
	c := p
	c.BodyJSON = bytes.Clone(p.BodyJSON)
	c.Opportunity = bytes.Clone(p.Opportunity)
	c.AudienceSnapshot = bytes.Clone(p.AudienceSnapshot)
	c.AudienceKeys = append(db.TextArray(nil), p.AudienceKeys...)
	return c
}

// Media is one post_media row (02 §12.3): an image of a post. It has no
// org_id; the store scopes it through its posts row (06 H-38).
type Media struct {
	ID        uuid.UUID `gorm:"type:uuid;column:id;primaryKey"`
	PostID    uuid.UUID `gorm:"type:uuid;column:post_id"`
	FileID    uuid.UUID `gorm:"type:uuid;column:file_id"`
	Position  int16     `gorm:"column:position"`
	AltText   *string   `gorm:"column:alt_text"`
	Width     *int      `gorm:"column:width"`
	Height    *int      `gorm:"column:height"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// TableName implements gorm's Tabler.
func (Media) TableName() string { return "post_media" }

// TrustLevel is feed_org_settings.trust_level (02 §12.1).
type TrustLevel string

// Trust levels. No settings row means new.
const (
	TrustNew        TrustLevel = "new"        // posts go to pending_review until enough are approved (T-15)
	TrustTrusted    TrustLevel = "trusted"    // posts are published at once
	TrustRestricted TrustLevel = "restricted" // every post is reviewed (BE-032 sets it)
	TrustSuspended  TrustLevel = "suspended"  // the org cannot publish (BE-032 sets it)
)

// OrgSettings is one feed_org_settings row (02 §12.1). Feed reads it in
// this ticket; moderation (BE-032) writes it.
type OrgSettings struct {
	OrgID              uuid.UUID  `gorm:"type:uuid;column:org_id;primaryKey"`
	TrustLevel         TrustLevel `gorm:"column:trust_level"`
	ApprovedPostsCount int        `gorm:"column:approved_posts_count"`
	RestrictedUntil    *time.Time `gorm:"column:restricted_until"`
	Reason             *string    `gorm:"column:reason"`
	UpdatedBy          *uuid.UUID `gorm:"type:uuid;column:updated_by"`
	CreatedAt          time.Time  `gorm:"column:created_at"`
	UpdatedAt          time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (OrgSettings) TableName() string { return "feed_org_settings" }

// Opportunity is posts.opportunity of an opportunity call (R-003 §1.6,
// 04 §F.1). It is a closed shape: unknown fields are refused.
type Opportunity struct {
	Type          string `json:"type"`                     // internship | job | part_time | project | volunteer | talent_call
	ApplyVia      string `json:"apply_via"`                // contact_request (companies) | external_url
	ExternalURL   string `json:"external_url,omitempty"`   // https:// only; required for external_url
	Deadline      string `json:"deadline,omitempty"`       // YYYY-MM-DD
	City          string `json:"city,omitempty"`           // ≤ 60 characters
	WorkMode      string `json:"work_mode,omitempty"`      // onsite | hybrid | remote
	EligibleYears []int  `json:"eligible_years,omitempty"` // class years 1–8
}

// PostSummary is the cross-module view of a post (01 §4.14 Posts.Get):
// messaging checks an opportunity call through it.
type PostSummary struct {
	ID          uuid.UUID
	OrgID       uuid.UUID
	Kind        Kind
	Status      Status
	Title       string
	Opportunity *Opportunity // opportunity_call only
}

// JSON is a jsonb column; nil is SQL NULL. Scan copies the driver's bytes.
type JSON []byte

// Value implements driver.Valuer.
func (j JSON) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return string(j), nil
}

// Scan implements sql.Scanner (jsonb arrives as text or bytes).
func (j *JSON) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = bytes.Clone(v)
	case string:
		*j = JSON(v)
	default:
		return fmt.Errorf("feed: jsonb: unexpected %T", src)
	}
	return nil
}

// FollowSource is org_follows.source (02 §12.7).
type FollowSource string

// Follow sources. auto_membership follows come from membership.started and
// go with membership.ended; a sensitive club never gets one (02 §12.7).
// auto_university is reserved by the schema (no producer in v1).
const (
	FollowManual         FollowSource = "manual"
	FollowAutoMembership FollowSource = "auto_membership"
	FollowAutoUniversity FollowSource = "auto_university"
)

// Follow is one org_follows row (02 §12.7): personal data of the follower;
// the org never sees who follows it.
type Follow struct {
	UserID    uuid.UUID    `gorm:"type:uuid;column:user_id"`
	OrgID     uuid.UUID    `gorm:"type:uuid;column:org_id"`
	Source    FollowSource `gorm:"column:source"`
	Notify    bool         `gorm:"column:notify"`
	CreatedAt time.Time    `gorm:"column:created_at"`
}

// Mute is one org_mutes row (02 §12.8): the org's posts leave the user's
// feed (the feed's "block", 04 §F.5).
type Mute struct {
	UserID    uuid.UUID `gorm:"type:uuid;column:user_id"`
	OrgID     uuid.UUID `gorm:"type:uuid;column:org_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// Interaction is a like, a save or a hide of a post by one user
// (post_reactions, post_saves, post_hides).
type Interaction struct {
	PostID    uuid.UUID `gorm:"type:uuid;column:post_id"`
	CreatedAt time.Time `gorm:"column:created_at"`
}

// SavedPost is a post of the reader's saved list with the time it was saved
// (the keyset of listMySavedPosts).
type SavedPost struct {
	Post    Post      `gorm:"embedded"`
	SavedAt time.Time `gorm:"column:saved_at"`
}

// Interactions is everything feed holds about a reader (KVKK export).
type Interactions struct {
	Likes   []Interaction
	Saves   []Interaction
	Hides   []Interaction
	Follows []Follow
	Mutes   []Mute
}
