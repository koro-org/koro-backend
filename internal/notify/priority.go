package notify

// Priority is the web push urgency of a template (BE-177): a template
// declares it in its meta file (priority: high | normal, default normal).
// Security alerts (a new session, a password, MFA, phone or e-mail change,
// a role change) and a new conversation message are high; everything else
// is normal, so the browsers' push services may batch them to save the
// device's battery. It sets the web push Urgency header (RFC 8030 §5.3),
// to browser subscriptions and to FCM web tokens (webpush.headers) alike.
//
// Mobile pushes do not follow it: FCM android.priority stays high and APNs
// apns-priority 10 for every message, as before BE-177 (a lower mobile
// priority would delay chat messages in Android Doze and change the mobile
// app's behaviour; out of scope).
//
// Only a template of the security preference category or one of
// highPriorityTemplates may be high.
type Priority string

// Push priorities.
const (
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
)

// highPriorityTemplates are the templates outside the security category
// allowed to be high: a message of an accepted conversation is read in
// real time.
var highPriorityTemplates = map[string]bool{"messaging.conversation_message": true}

func (p Priority) valid() bool { return p == PriorityNormal || p == PriorityHigh }

// webPushUrgency is the Urgency header of a web push (RFC 8030 §5.3); the
// zero value is normal.
func (p Priority) webPushUrgency() string {
	if p == PriorityHigh {
		return "high"
	}
	return "normal"
}

// Mobile priorities: fixed, whatever the template's priority (see Priority).
const (
	androidPriority = "high"
	apnsPriority    = "10"
)
