package notify

import (
	"context"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// prefResource is the authz view of a user's preferences (03 §3.5).
func prefResource(owner uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeNotificationPref, ID: owner, OwnerUserID: &owner}
}

// Allowed implements Preferences. Security notifications are always
// allowed (02 §6.5 CHECK); a category without a stored choice is at its
// default. An unknown category (a programmer error) is allowed: preferences
// only ever suppress what the user explicitly switched off.
func (s *service) Allowed(ctx context.Context, userID uuid.UUID, category PrefCategory, ch Channel) (bool, error) {
	if category == PrefSecurity || !category.valid() || !ch.valid() {
		return true, nil
	}
	enabled, ok, err := s.d.Store.Preference(ctx, userID, category, ch)
	if err != nil {
		return false, err
	}
	if !ok {
		return DefaultEnabled(category, ch), nil
	}
	return enabled, nil
}

// MyPreferences implements Service: all 21 (category, channel) pairs in
// 02 §6.5 × 02 §6.1 order, the stored choice or the default.
func (s *service) MyPreferences(ctx context.Context, a authz.Actor) ([]Preference, error) {
	if err := authz.Can(ctx, a, authz.NotifyPreferenceReadOwn, prefResource(a.UserID)); err != nil {
		return nil, err
	}
	return s.preferencesOf(ctx, a.UserID)
}

func (s *service) preferencesOf(ctx context.Context, userID uuid.UUID) ([]Preference, error) {
	stored, err := s.d.Store.Preferences(ctx, userID)
	if err != nil {
		return nil, err
	}
	set := map[[2]string]bool{}
	for _, p := range stored {
		set[[2]string{string(p.Category), string(p.Channel)}] = p.Enabled
	}
	out := make([]Preference, 0, len(PrefCategories())*len(Channels()))
	for _, c := range PrefCategories() {
		for _, ch := range Channels() {
			enabled, ok := set[[2]string{string(c), string(ch)}]
			switch {
			case c == PrefSecurity:
				enabled = true
			case !ok:
				enabled = DefaultEnabled(c, ch)
			}
			out = append(out, Preference{Category: c, Channel: ch, Enabled: enabled})
		}
	}
	return out, nil
}

// PutMyPreferences implements Service. Only the given pairs change; the
// rest keep their value. Switching a security pair off is 422
// notify.security_preference_locked; a pair listed twice is 422. The change
// and its audit record share one transaction.
func (s *service) PutMyPreferences(ctx context.Context, a authz.Actor, items []Preference) ([]Preference, error) {
	if err := authz.Can(ctx, a, authz.NotifyPreferenceUpdateOwn, prefResource(a.UserID)); err != nil {
		return nil, err
	}
	seen := map[[2]string]bool{}
	for _, p := range items {
		k := [2]string{string(p.Category), string(p.Channel)}
		switch {
		case !p.Category.valid() || !p.Channel.valid():
			return nil, problem.ErrValidation.WithFields(problem.FieldError{Location: "body.items", Message: "unknown category or channel"})
		case seen[k]:
			return nil, problem.ErrValidation.WithFields(problem.FieldError{Location: "body.items", Message: "each (category, channel) pair may appear once"})
		case p.Category == PrefSecurity && !p.Enabled:
			return nil, ErrSecurityLocked.WithFields(problem.FieldError{Location: "body.items", Message: "security notifications cannot be switched off"})
		}
		seen[k] = true
	}
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.UpsertPreferences(ctx, a.UserID, items, s.d.Clock.Now()); err != nil {
			return err
		}
		off := 0
		for _, p := range items {
			if !p.Enabled {
				off++
			}
		}
		owner := a.UserID
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.NotifyPreferenceUpdateOwn), Outcome: audit.Success,
			ResourceType: authz.TypeNotificationPref, ResourceID: &owner,
			Metadata: map[string]any{"changed": len(items), "disabled": off},
		}.By(a))
	})
	if err != nil {
		return nil, err
	}
	return s.preferencesOf(ctx, a.UserID)
}
