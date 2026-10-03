package notify

import (
	"time"
	"uuid"
)

// PrefCategory is a user-facing notification category of the preferences
// (notification_preferences.category, 02 §6.5). Every transactional
// template declares one in its meta file; commercial messages are governed
// by consent, never by these categories.
type PrefCategory string

// Preference categories (02 §6.5).
const (
	// PrefSecurity cannot be switched off (02 §6.5 CHECK; 422).
	PrefSecurity            PrefCategory = "security"
	PrefRegistrationUpdates PrefCategory = "registration_updates"
	PrefEventUpdates        PrefCategory = "event_updates"
	PrefContactRequests     PrefCategory = "contact_requests"
	PrefFeedFollows         PrefCategory = "feed_follows"
	PrefOrdersPayments      PrefCategory = "orders_payments"
	PrefModeration          PrefCategory = "moderation"
)

// PrefCategories returns every category in 02 §6.5 order.
func PrefCategories() []PrefCategory {
	return []PrefCategory{PrefSecurity, PrefRegistrationUpdates, PrefEventUpdates, PrefContactRequests,
		PrefFeedFollows, PrefOrdersPayments, PrefModeration}
}

func (c PrefCategory) valid() bool {
	for _, v := range PrefCategories() {
		if c == v {
			return true
		}
	}
	return false
}

// Channels returns every delivery channel in 02 §6.1 order.
func Channels() []Channel { return []Channel{ChannelEmail, ChannelPush, ChannelInApp} }

// Preference is one (category, channel) switch of a user. A missing row
// means DefaultEnabled.
type Preference struct {
	Category PrefCategory
	Channel  Channel
	Enabled  bool
}

// DefaultEnabled is the value of a (category, channel) pair the user never
// set (02 §6.5: "satır yoksa kategori varsayılanı geçerlidir (kodda)").
// Everything is on except feed follow e-mails: following an org yields push
// summaries (01 §6.2 post.published), not mail, unless the user asks for it.
func DefaultEnabled(c PrefCategory, ch Channel) bool {
	return c != PrefFeedFollows || ch != ChannelEmail
}

// InAppNotification is one in_app_notifications row (02 §6.4). Title and
// Body come from the template (fixed text, no personal data); Data holds
// opaque IDs only.
type InAppNotification struct {
	ID          uuid.UUID // = the delivery ID: a retried send writes the same row
	UserID      uuid.UUID
	Kind        string // the template ID
	Title       string
	Body        string
	LinkPath    string
	SourceOrgID *uuid.UUID
	Data        map[string]string
	ReadAt      *time.Time
	CreatedAt   time.Time
}

// PushPlatform is push_tokens.platform.
type PushPlatform string

// Push platforms (02 §6.3).
const (
	PushIOS     PushPlatform = "ios"
	PushAndroid PushPlatform = "android"
	PushWeb     PushPlatform = "web"
)

func (p PushPlatform) valid() bool { return p == PushIOS || p == PushAndroid || p == PushWeb }

// PushToken is one push_tokens row (02 §6.3). Token is the device secret:
// it is never returned by the API, logged or exported.
type PushToken struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	SessionID   *uuid.UUID
	Platform    PushPlatform
	Token       string
	TokenSHA256 []byte
	AppVersion  string
	CreatedAt   time.Time
	LastSeenAt  time.Time
	RevokedAt   *time.Time
}

// SuppressionReason is email_suppressions.reason.
type SuppressionReason string

// Suppression reasons (02 §6.2).
const (
	SuppressHardBounce SuppressionReason = "hard_bounce"
	SuppressComplaint  SuppressionReason = "complaint"
	SuppressManual     SuppressionReason = "manual"
	SuppressInvalid    SuppressionReason = "invalid"
)

// SuppressionSource is email_suppressions.source.
type SuppressionSource string

// Suppression sources (02 §6.2).
const (
	SourceProviderWebhook SuppressionSource = "provider_webhook"
	SourceAdmin           SuppressionSource = "admin"
	SourceUserRequest     SuppressionSource = "user_request"
)

// Suppression is one email_suppressions row: an address (kept only as its
// blind index) that must not receive e-mail. Its API ID is the hex of the
// index (SuppressionID).
type Suppression struct {
	EmailIndex []byte // 32 bytes
	Reason     SuppressionReason
	Source     SuppressionSource
	CreatedAt  time.Time
}
