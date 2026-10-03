package notify

import "time"

// Response DTOs of the notify operations (04 §2.19). Input DTOs are copied
// field by field into the service types (05 §14.4: no mass assignment).

// SourceOrgDTO names the org an in-app notification comes from. Only the ID:
// the org's display name belongs to orgs (L5, above notify).
type SourceOrgDTO struct {
	OrgID string `json:"org_id" format:"uuid"`
}

// NotificationDTO is one in-app notification.
type NotificationDTO struct {
	NotificationID string            `json:"notification_id" format:"uuid"`
	Kind           string            `json:"kind" doc:"The notification kind (template ID)"`
	Title          string            `json:"title"`
	Body           string            `json:"body,omitempty"`
	LinkPath       string            `json:"link_path,omitempty" doc:"In-app relative path"`
	SourceOrg      *SourceOrgDTO     `json:"source_org,omitempty"`
	Data           map[string]string `json:"data,omitempty" doc:"Opaque IDs only"`
	CreatedAt      time.Time         `json:"created_at"`
	ReadAt         *time.Time        `json:"read_at,omitempty"`
}

// NotificationList is a page of in-app notifications.
type NotificationList struct {
	Items      []NotificationDTO `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func toNotificationDTO(n InAppNotification) NotificationDTO {
	d := NotificationDTO{NotificationID: n.ID.String(), Kind: n.Kind, Title: n.Title, Body: n.Body, LinkPath: n.LinkPath,
		Data: n.Data, CreatedAt: n.CreatedAt.UTC(), ReadAt: timeOrNil(n.ReadAt)}
	if len(d.Data) == 0 {
		d.Data = nil
	}
	if n.SourceOrgID != nil {
		d.SourceOrg = &SourceOrgDTO{OrgID: n.SourceOrgID.String()}
	}
	return d
}

// PreferenceDTO is one (category, channel) switch.
type PreferenceDTO struct {
	Category string `json:"category" enum:"security,registration_updates,event_updates,contact_requests,feed_follows,orders_payments,moderation"`
	Channel  string `json:"channel" enum:"email,push,in_app"`
	Enabled  bool   `json:"enabled"`
}

// PreferenceList is every pair of the user, defaults included.
type PreferenceList struct {
	Items []PreferenceDTO `json:"items"`
}

func toPreferenceList(prefs []Preference) PreferenceList {
	out := PreferenceList{Items: make([]PreferenceDTO, len(prefs))}
	for i, p := range prefs {
		out.Items[i] = PreferenceDTO{Category: string(p.Category), Channel: string(p.Channel), Enabled: p.Enabled}
	}
	return out
}

// PushTokenCreatedDTO answers registerMyPushToken.
type PushTokenCreatedDTO struct {
	PushTokenID string `json:"push_token_id" format:"uuid"`
}

// SuppressionDTO is one suppressed address (no address: only its ID).
type SuppressionDTO struct {
	SuppressionID string    `json:"suppression_id"`
	Reason        string    `json:"reason" enum:"hard_bounce,complaint,manual,invalid"`
	Source        string    `json:"source" enum:"provider_webhook,admin,user_request"`
	CreatedAt     time.Time `json:"created_at"`
}

// SuppressionList is a page of suppressions.
type SuppressionList struct {
	Items      []SuppressionDTO `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

// DeliveryDTO is one delivery record as support sees it (04 §2.19): never
// the content, the template data, the address, its index or the provider
// error.
type DeliveryDTO struct {
	DeliveryID string    `json:"delivery_id" format:"uuid"`
	Channel    string    `json:"channel" enum:"email,push,in_app"`
	Template   string    `json:"template"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// DeliveryList is a page of deliveries.
type DeliveryList struct {
	Items      []DeliveryDTO `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func toDeliveryDTO(d Delivery) DeliveryDTO {
	return DeliveryDTO{DeliveryID: d.ID.String(), Channel: string(d.Channel), Template: d.Template, Status: string(d.Status),
		CreatedAt: d.CreatedAt.UTC()}
}
