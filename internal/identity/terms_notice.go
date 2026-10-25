package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"uuid"

	"koro/internal/consent"
	"koro/internal/notify"
	"koro/internal/platform/applink"
	"koro/internal/platform/db"
	"koro/internal/platform/queue"
)

// The notice of a new terms of use version (BE-124, Q-17): when consent
// publishes a terms_of_use version (adminPublishNotice), every active
// reader it is now in force for — their audience and locale resolve to it
// (consent.Notices.Current, with its fallbacks) — gets an in-app
// notification and a transactional e-mail (identity.terms_updated,
// preference security: a contract notice cannot be switched off) asking
// them to read and accept it. Until they do, the terms gate refuses their
// writes (terms_gate.go).
//
// consent tells identity in the publishing transaction (TermsPublished,
// consent.TermsWatcher); the fan-out task is enqueued after the commit
// (mechanism A: no event topic, the publication is the trigger) and
// queues one task per reader, each deduplicated per (version, reader), so
// a retry sends nothing twice. A reader who accepted meanwhile, or whose
// version in force changed again, gets nothing.
const (
	// NotifyTermsPublishedTaskName fans a published version out to its
	// readers (one identity.send_terms_notice per reader).
	NotifyTermsPublishedTaskName = "identity.notify_terms_published"
	// SendTermsNoticeTaskName sends one reader the notice (in-app, e-mail).
	SendTermsNoticeTaskName = "identity.send_terms_notice"

	termsNoticeTemplate = "identity.terms_updated"
	termsBatch          = 1000 // accounts read per batch of the fan-out (01 §7)
)

// termsLink is the client path of the terms acceptance (notify's catalog,
// BE-122): the in-app deep link, and on APP_LINK_BASE_URL the e-mail link
// (00 T-377). koro-web's TermsGate shows the acceptance on every signed-in
// page.
var termsLink = notify.MyTermsLink()

// termsPublished is the payload of identity.notify_terms_published.
type termsPublished struct {
	NoticeID uuid.UUID `json:"notice_id"`
}

// termsNotice is the payload of identity.send_terms_notice (IDs only).
type termsNotice struct {
	UserID   uuid.UUID `json:"user_id"`
	NoticeID uuid.UUID `json:"notice_id"`
}

// dedupe is the task ID and the stem of notify's deduplication keys: one
// notice per version and reader.
func (n termsNotice) dedupe() string {
	return "identity.terms_notice:" + n.NoticeID.String() + ":" + n.UserID.String()
}

// TermsPublished implements consent.TermsWatcher (bound in
// cmd/koro/wire_identity.go): after the publishing transaction commits,
// this process's terms gate drops its versions in force and the fan-out
// is queued. A publication that rolls back does neither.
func (m Module) TermsPublished(ctx context.Context, noticeID uuid.UUID) {
	s := m.svc
	db.AfterCommit(ctx, func(context.Context) { s.gate.dropInForce() })
	s.d.Queue.AfterCommit(ctx, NotifyTermsPublishedTaskName, termsPublished{NoticeID: noticeID},
		NotifyTermsPublishedTaskName+":"+noticeID.String())
}

// termsReader is the (persona, locale) of an account: what the version in
// force is resolved for.
type termsReader struct {
	persona Persona
	locale  string
}

// termsPersonas are the personas an account can have (signup personas and
// the seeded platform staff).
var termsPersonas = []Persona{PersonaStudent, PersonaCompany, PersonaUniversity, PersonaClub, PersonaPlatform}

// readersOf returns the (persona, locale) pairs noticeID is the version in
// force for (none: another version superseded it before the task ran).
func (s *service) readersOf(ctx context.Context, noticeID uuid.UUID) (map[termsReader]bool, error) {
	out := map[termsReader]bool{}
	for _, p := range termsPersonas {
		for _, l := range []string{LocaleTR, LocaleEN} {
			n, err := s.d.Terms.Current(ctx, consent.NoticeKey{Kind: consent.KindTermsOfUse, Audience: audienceOf(p), Locale: l})
			switch {
			case errors.Is(err, consent.ErrNoNotice):
			case err != nil:
				return nil, err
			case n.ID == noticeID:
				out[termsReader{persona: p, locale: l}] = true
			}
		}
	}
	return out, nil
}

// handleTermsPublished is identity.notify_terms_published: one
// identity.send_terms_notice per active account the version is in force
// for, the accounts read in batches of termsBatch (newest first).
func (s *service) handleTermsPublished(ctx context.Context, payload []byte) error {
	var p termsPublished
	if err := json.Unmarshal(payload, &p); err != nil || p.NoticeID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("identity: terms published payload: %w", errors.Join(err, errors.New("missing notice id"))))
	}
	readers, err := s.readersOf(ctx, p.NoticeID)
	if err != nil {
		return err
	}
	queued := 0
	if len(readers) > 0 {
		var after *uuid.UUID
		if _, err := db.Drain(ctx, termsBatch, func(ctx context.Context) (int, error) {
			users, err := s.d.Store.SearchUsers(ctx, UserSearch{Status: StatusActive, After: after, Limit: termsBatch})
			if err != nil {
				return 0, err
			}
			for _, u := range users {
				if !readers[termsReader{persona: u.Persona, locale: u.Locale}] {
					continue
				}
				n := termsNotice{UserID: u.ID, NoticeID: p.NoticeID}
				if err := s.d.Queue.Enqueue(ctx, SendTermsNoticeTaskName, n, n.dedupe()); err != nil {
					return 0, err
				}
				queued++
			}
			if len(users) > 0 {
				last := users[len(users)-1].ID
				after = &last
			}
			return len(users), nil
		}); err != nil {
			return fmt.Errorf("%s: %w", NotifyTermsPublishedTaskName, err)
		}
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "terms of use notices queued", slog.String("task", NotifyTermsPublishedTaskName),
		slog.String("notice_id", p.NoticeID.String()), slog.Int("reader_kinds", len(readers)), slog.Int("queued", queued))
	return nil
}

// handleTermsNotice is identity.send_terms_notice: the in-app notification
// and the e-mail of one reader who still has to accept the version.
func (s *service) handleTermsNotice(ctx context.Context, payload []byte) error {
	var n termsNotice
	if err := json.Unmarshal(payload, &n); err != nil || n.UserID == uuid.Nil() || n.NoticeID == uuid.Nil() {
		return queue.Permanent(fmt.Errorf("identity: terms notice payload: %w", errors.Join(err, errors.New("missing ids"))))
	}
	u, err := s.d.Store.User(ctx, n.UserID)
	switch {
	case isNotFound(err):
		return nil
	case err != nil:
		return err
	}
	if u.Status != StatusActive || u.Email == "" {
		return nil
	}
	st, err := s.termsState(ctx, u)
	if err != nil {
		return err
	}
	if !st.Required || st.CurrentID == nil || *st.CurrentID != n.NoticeID {
		return nil // accepted meanwhile, or another version is in force now
	}
	data := map[string]any{"name": u.FirstName, "version": st.CurrentVersion, "link": applink.Page(s.d.LinkBaseURL, termsLink.String())}
	for _, m := range []notify.Message{
		{UserID: &u.ID, Channel: notify.ChannelInApp, Category: notify.CategoryTransactional, Template: termsNoticeTemplate,
			Locale: u.Locale, Data: map[string]any{"notice_version_id": n.NoticeID.String()}, LinkPath: termsLink,
			DedupeKey: n.dedupe() + ":in_app"},
		{UserID: &u.ID, ToEmail: u.Email, Channel: notify.ChannelEmail, Category: notify.CategoryTransactional,
			Template: termsNoticeTemplate, Locale: u.Locale, Data: data, DedupeKey: n.dedupe() + ":email"},
	} {
		if _, err := s.d.Mailer.Send(ctx, m); err != nil {
			if notify.IsPermanent(err) {
				return queue.Permanent(err)
			}
			return err
		}
	}
	return nil
}
