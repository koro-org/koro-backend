package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

const maxLastError = 2000 // notification_deliveries.last_error CHECK

type service struct{ d Deps }

func (s *service) Send(ctx context.Context, m Message) (uuid.UUID, error) {
	// Side effects belong to the worker (01 §0.5). A request carries a request
	// ID and no task ID.
	if obs.RequestID(ctx) != "" && obs.TaskID(ctx) == "" {
		return uuid.Nil(), ErrNotInWorker
	}
	// Commercial messages never leave without a consent check: refuse before
	// anything else, even an otherwise invalid message.
	if m.Category == CategoryCommercial && s.d.Consent == nil {
		return uuid.Nil(), ErrCommercialDisabled
	}
	if fe := validateMessage(m); len(fe) > 0 {
		return uuid.Nil(), ErrInvalidMessage.WithFields(fe...)
	}

	// Render before claiming the dedupe key: a message that cannot be rendered
	// must not leave a delivery row behind.
	c, err := s.prepare(m)
	if err != nil {
		return uuid.Nil(), err
	}

	now := s.d.Clock.Now()
	d, state, err := s.d.Store.Claim(ctx, Delivery{
		ID: uuid.NewV7(), UserID: m.UserID, RecipientIndex: s.recipientIndex(m),
		Channel: m.Channel, Category: m.Category, Template: m.Template,
		TemplateVersion: c.meta.Version, DedupeKey: m.DedupeKey,
	}, now, s.d.Lease)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("notify: claim delivery: %w", err)
	}
	switch state {
	case ClaimDone:
		s.log(ctx, slog.LevelDebug, "notification already handled", d, m)
		return d.ID, nil
	case ClaimInFlight:
		s.log(ctx, slog.LevelInfo, "notification attempt in progress elsewhere", d, m)
		return d.ID, ErrDeliveryInFlight
	}

	result, sendErr := s.deliver(ctx, m, c, d)

	// The outcome is recorded even if the context was cancelled meanwhile.
	if _, err := s.d.Store.Finish(context.WithoutCancel(ctx), d.ID, result, s.d.Clock.Now()); err != nil {
		// The provider already has the message when result.Status is sent:
		// failing the task would only send it twice. Log and report the send.
		s.log(ctx, slog.LevelError, "notification outcome not recorded", d, m,
			slog.String("error", obs.Scrub(err.Error())), slog.String("status", string(result.Status)))
		if sendErr != nil {
			return d.ID, sendErr
		}
		return d.ID, nil
	}

	switch {
	case sendErr != nil:
		s.log(ctx, slog.LevelWarn, "notification delivery failed", d, m,
			slog.String("status", string(result.Status)), slog.String("error", result.LastError))
	default:
		s.log(ctx, slog.LevelInfo, "notification handled", d, m, slog.String("status", string(result.Status)))
	}
	return d.ID, sendErr
}

// content is a message prepared for delivery: the template's meta and
// either the rendered e-mail or the fixed in_app/push text.
type content struct {
	meta   TemplateMeta
	email  Rendered
	notice Notice
}

// prepare checks the message against its template (known, serves the
// channel, same category) and renders it.
func (s *service) prepare(m Message) (content, error) {
	meta, ok := s.d.Templates.Meta(m.Template)
	if !ok {
		return content{}, ErrInvalidMessage.WithFields(problem.FieldError{Location: "message.template", Message: "unknown template"})
	}
	var fe []problem.FieldError
	if !meta.Supports(m.Channel) {
		fe = append(fe, problem.FieldError{Location: "message.channel", Message: "the template does not serve this channel"})
	}
	if meta.Category != m.Category {
		fe = append(fe, problem.FieldError{Location: "message.category", Message: "does not match the category declared by the template"})
	}
	if len(fe) > 0 {
		return content{}, ErrInvalidMessage.WithFields(fe...)
	}
	c := content{meta: meta}
	if m.Channel != ChannelEmail {
		c.notice, _ = s.d.Templates.Notice(m.Template, m.Locale) // served: Supports checked above
		return c, nil
	}
	r, err := s.d.Templates.Render(m.Template, m.Locale, RenderInput{Data: m.Data, UnsubscribeURL: m.UnsubscribeURL})
	if err != nil {
		return content{}, err
	}
	if fe := checkInlineRefs(m, r.HTML); len(fe) > 0 {
		return content{}, ErrInvalidMessage.WithFields(fe...)
	}
	c.email = r
	return c, nil
}

// deliver performs one attempt for a claimed delivery and returns the result
// to record plus the error for the caller.
func (s *service) deliver(ctx context.Context, m Message, c content, d Delivery) (Result, error) {
	skip, err := s.gate(ctx, m, c.meta, d)
	if err != nil {
		return failure("", err)
	}
	if skip != "" {
		return Result{Status: skip}, nil
	}

	var provider, providerID string
	switch m.Channel {
	case ChannelEmail:
		provider = s.d.Transport.Name()
		providerID, err = s.d.Transport.Send(ctx, composeEmail(s.d.FromAddress, s.d.FromName, d.ID, s.d.Clock.Now(), m, c.email))
	case ChannelPush:
		provider = s.d.Push.Name()
		providerID, err = s.d.Push.SendPush(ctx, PushMessage{
			DeliveryID: d.ID, UserID: *m.UserID, Kind: m.Template, Title: c.notice.Title, Body: c.notice.Body,
			LinkPath: m.LinkPath, Data: opaqueData(m.Data),
		})
	case ChannelInApp:
		provider, providerID = inAppProvider, d.ID.String()
		_, err = s.d.Store.InsertInApp(ctx, InAppNotification{
			ID: d.ID, UserID: *m.UserID, Kind: m.Template, Title: c.notice.Title, Body: c.notice.Body,
			LinkPath: m.LinkPath, SourceOrgID: m.SourceOrgID, Data: opaqueData(m.Data), CreatedAt: s.d.Clock.Now(),
		})
	}
	if err != nil {
		return failure(provider, err)
	}
	sentAt := s.d.Clock.Now()
	return Result{Status: StatusSent, Provider: provider, ProviderMessageID: providerID, SentAt: &sentAt}, nil
}

// inAppProvider is the provider of in-app deliveries (the inbox table itself).
const inAppProvider = "in_app"

// gate decides whether a claimed message may go out. It returns the status
// of a deliberate skip ("" = send):
//
//   - e-mail to a suppressed address → suppressed (hard bounce, complaint);
//   - commercial without a consent in force → skipped_no_consent (6563;
//     consent.Checker.FilterActive);
//   - transactional message to a user who switched its preference category
//     off on this channel → skipped_preference (security never).
//
// Consent and preferences are disjoint: commercial messages ignore the
// preferences, transactional ones never ask for consent.
func (s *service) gate(ctx context.Context, m Message, meta TemplateMeta, d Delivery) (Status, error) {
	if m.Channel == ChannelEmail {
		suppressed, err := s.d.Store.IsSuppressed(ctx, d.RecipientIndex)
		if err != nil {
			return "", err
		}
		if suppressed {
			return StatusSuppressed, nil
		}
	}
	switch {
	case m.Category == CategoryCommercial:
		purpose := PurposeCommercialEmail
		if m.Channel == ChannelPush {
			purpose = PurposeCommercialPush
		}
		ok, err := s.consented(ctx, *m.UserID, purpose)
		if err != nil {
			return "", err
		}
		if !ok {
			return StatusSkippedNoConsent, nil
		}
	case m.UserID != nil:
		ok, err := s.Allowed(ctx, *m.UserID, meta.Preference, m.Channel)
		if err != nil {
			return "", err
		}
		if !ok {
			return StatusSkippedPreference, nil
		}
	}
	return "", nil
}

// consented asks the consent port for one user.
func (s *service) consented(ctx context.Context, userID uuid.UUID, purpose string) (bool, error) {
	active, err := s.d.Consent.FilterActive(ctx, []uuid.UUID{userID}, purpose)
	if err != nil {
		return false, err
	}
	return slices.Contains(active, userID), nil
}

// failure turns a provider or consent error into the record to store and the
// error to return. Provider errors may contain the recipient address
// ("550 5.1.1 <ayse@…>: user unknown"), so the text is scrubbed before it
// reaches the store, the logs or the caller.
func failure(provider string, err error) (Result, error) {
	msg := truncate(obs.Scrub(err.Error()), maxLastError)
	res := Result{Status: StatusFailed, Provider: provider, LastError: msg}
	cause := errors.New(msg) // detached from err: its text may hold personal data
	if errors.Is(err, ErrPermanent) {
		return res, ErrDeliveryRejected.Wrap(cause)
	}
	return res, ErrDeliveryFailed.Wrap(cause)
}

func (s *service) recipientIndex(m Message) []byte {
	if m.Channel != ChannelEmail {
		return nil
	}
	return s.indexOf(m.ToEmail)
}

// indexOf is the blind index of an address (02 §6.1, §6.2).
func (s *service) indexOf(email string) []byte {
	return s.d.Recipients.Index(strings.ToLower(strings.TrimSpace(email)))
}

// log writes one structured line. It carries IDs, names and the masked
// recipient only: never the subject, the body, Data or the raw address.
func (s *service) log(ctx context.Context, level slog.Level, msg string, d Delivery, m Message, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("delivery_id", d.ID.String()),
		slog.String("channel", string(d.Channel)),
		slog.String("category", string(d.Category)),
		slog.String("template", d.Template),
		slog.Int("template_version", d.TemplateVersion),
		slog.Int("attempts", d.Attempts),
	}
	if d.UserID != nil {
		attrs = append(attrs, slog.String("user_id", d.UserID.String()))
	}
	if m.Channel == ChannelEmail {
		attrs = append(attrs, slog.Any("recipient", obs.Email(m.ToEmail)))
	}
	s.d.Log.LogAttrs(ctx, level, msg, append(attrs, extra...)...)
}

// truncate shortens s to at most n bytes without cutting a rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
