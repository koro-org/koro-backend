package talent

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
	"uuid"
)

// Signal codes of talent.signal_defs (02 §15.2, seed 000081). The weights
// and caps are data (a product decision tuned with pilot data), never code.
const (
	SignalEventCheckin          = "event_checkin"
	SignalRegistrationConfirmed = "event_registration_confirmed"
	SignalEventNoShow           = "event_no_show"
	SignalProfileSection        = "profile_section_complete"
	SignalClubMembership        = "club_membership_active"
	SignalFeedLike              = "feed_like"
	SignalFeedSave              = "feed_save"
	SignalActiveDay             = "login_active_day"
)

// Source types of talent.point_events.source_type: with the user, the
// signal and the source ID they are the dedupe key that makes every
// consumer idempotent (point_events_dedupe_uq).
const (
	SourceCheckin        = "checkin"
	SourceRegistration   = "registration"
	SourceMembership     = "membership"
	SourcePost           = "post"
	SourceProfileSection = "profile_section"
	SourceDay            = "day"
	SourceReversal       = "reversal"
)

// Cap reasons of talent.point_events.cap_reason.
const (
	CapDaily     = "daily"
	CapWeekly    = "weekly"
	CapPerSource = "per_source"
)

// Categories of talent.signal_defs.category.
const (
	CategoryParticipation = "participation"
	CategoryIntent        = "intent"
	CategoryProfile       = "profile"
	CategoryCommunity     = "community"
	CategoryEngagement    = "engagement"
)

// maxActiveClubs is the "at most 2 clubs" rule of club_membership_active
// (seed 000081): a third concurrent club is recorded with 0 points
// (cap per_source).
const maxActiveClubs = 2

// SignalDef is one talent.signal_defs row. Caps are magnitudes in cp
// (1/100 point); nil means no cap of that period.
type SignalDef struct {
	Code                  string
	Category              string
	BasePointsCP          int
	DailyCapCP            *int
	WeeklyCapCP           *int
	HalfLifeDays          *int
	ExcludedWhenSensitive bool
	Enabled               bool
}

// ConfigVersion is one talent.config_versions row (immutable). Every
// ledger row is frozen with the version in force when it was written.
type ConfigVersion struct {
	ID            uuid.UUID
	EffectiveFrom time.Time
	Params        json.RawMessage
	Notes         string
	CreatedBy     uuid.UUID
	CreatedAt     time.Time
}

// SourceKey is the dedupe key of a ledger row beside the user.
type SourceKey struct {
	UserID     uuid.UUID
	Signal     string
	SourceType string
	SourceID   uuid.UUID
}

// PointEvent is one talent.point_events row (append-only; a correction is
// a reversal row pointing at the original).
type PointEvent struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Signal          string
	SourceType      string
	SourceID        uuid.UUID
	OccurredAt      time.Time
	RawPointsCP     int
	PointsCP        int
	Capped          bool
	CapReason       string // "" when not capped
	ReasonCode      string
	ConfigVersionID uuid.UUID
	ReversalOf      *uuid.UUID
	SourceEventID   uuid.UUID
	CreatedAt       time.Time
}

// Key returns the dedupe key of p.
func (p PointEvent) Key() SourceKey {
	return SourceKey{UserID: p.UserID, Signal: p.Signal, SourceType: p.SourceType, SourceID: p.SourceID}
}

// NeutralizeReason is talent.neutralizations.reason.
type NeutralizeReason string

// Neutralisation reasons (02 §15.8). A minor needs none: a minor produces
// no signal at all (T-22, T-23).
const (
	// NeutralizeOptOut: the student turned activity based ranking off
	// (student_privacy_settings.activity_ranking_choice = disabled); lifted
	// when the student turns it on again, never by staff.
	NeutralizeOptOut NeutralizeReason = "opt_out"
	// NeutralizeObjection: a KVKK objection to profiling (md. 11/1-g,
	// dsr.profiling_objection). Permanent: no staff member lifts it.
	NeutralizeObjection NeutralizeReason = "objection"
	// NeutralizeAdmin: a talent_auditor decision (BE-052); liftable.
	NeutralizeAdmin NeutralizeReason = "admin"
)

// Valid reports whether r is one of the reasons of the CHECK.
func (r NeutralizeReason) Valid() bool {
	return r == NeutralizeOptOut || r == NeutralizeObjection || r == NeutralizeAdmin
}

// stopsCollection reports whether an active neutralisation of reason r
// stops the ledger: an opt-out and an objection end the processing of the
// activity (data minimisation); an auditor's neutralisation only zeroes
// the band.
func (r NeutralizeReason) stopsCollection() bool {
	return r == NeutralizeOptOut || r == NeutralizeObjection
}

// Neutralization is one talent.neutralizations row: while it is active
// (LiftedAt nil) the user's band is 0.
type Neutralization struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Reason    NeutralizeReason
	DSRID     *uuid.UUID
	Note      string
	CreatedBy *uuid.UUID
	CreatedAt time.Time
	LiftedAt  *time.Time
	LiftedBy  *uuid.UUID
}

// Active reports whether n is in force.
func (n Neutralization) Active() bool { return n.LiftedAt == nil }

// Liftable reports whether staff may lift n (talent.neutralization.manage,
// BE-052): only an auditor's own neutralisation. An objection is
// permanent (proof of compliance with the KVKK objection, 02 §15.8) and an
// opt-out belongs to the student, who lifts it through the privacy
// setting.
func (n Neutralization) Liftable() error {
	switch {
	case !n.Active():
		return ErrNotActive
	case n.Reason == NeutralizeObjection:
		return ErrObjectionPermanent
	case n.Reason == NeutralizeOptOut:
		return ErrOptOutStudentOwned
	}
	return nil
}

// derivedID is a stable UUID (version 8, RFC 9562 custom) of a source that
// has no ID of its own: a profile section of a user, an active day. The
// same parts always give the same ID, so a repeated event hits the dedupe
// key. It is not secret and identifies nothing beyond its parts.
func derivedID(parts ...string) uuid.UUID {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	var u uuid.UUID
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x80 // version 8
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 9562 variant
	return u
}
