package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	rendered, version, err := s.render(m)
	if err != nil {
		return uuid.Nil(), err
	}

	now := s.d.Clock.Now()
	d, state, err := s.d.Store.Claim(ctx, Delivery{
		ID: uuid.NewV7(), UserID: m.UserID, RecipientIndex: s.recipientIndex(m),
		Channel: m.Channel, Category: m.Category, Template: m.Template,
		TemplateVersion: version, DedupeKey: m.DedupeKey,
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

	result, sendErr := s.deliver(ctx, m, rendered, d)

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

// render renders the e-mail body. Push and in-app templates are rendered by
// their adapters (BE-055, BE-015); they record version 1.
func (s *service) render(m Message) (Rendered, int, error) {
	if m.Channel != ChannelEmail {
		return Rendered{}, 1, nil
	}
	r, err := s.d.Templates.Render(m.Template, m.Locale, RenderInput{Data: m.Data, UnsubscribeURL: m.UnsubscribeURL})
	if err != nil {
		return Rendered{}, 0, err
	}
	var fe []problem.FieldError
	if r.Category != m.Category {
		fe = append(fe, problem.FieldError{Location: "message.category", Message: "does not match the category declared by the template"})
	}
	fe = append(fe, checkInlineRefs(m, r.HTML)...)
	if len(fe) > 0 {
		return Rendered{}, 0, ErrInvalidMessage.WithFields(fe...)
	}
	return r, r.Version, nil
}

// deliver performs one attempt for a claimed delivery and returns the result
// to record plus the error for the caller.
func (s *service) deliver(ctx context.Context, m Message, r Rendered, d Delivery) (Result, error) {
	if m.Category == CategoryCommercial {
		purpose := PurposeCommercialEmail
		if m.Channel == ChannelPush {
			purpose = PurposeCommercialPush
		}
		ok, err := s.d.Consent.HasActive(ctx, *m.UserID, purpose)
		if err != nil {
			return failure("", err)
		}
		if !ok {
			return Result{Status: StatusSkippedNoConsent}, nil
		}
	}

	var provider, providerID string
	var err error
	switch m.Channel {
	case ChannelEmail:
		provider = s.d.Transport.Name()
		providerID, err = s.d.Transport.Send(ctx, composeEmail(s.d.FromAddress, s.d.FromName, d.ID, s.d.Clock.Now(), m, r))
	case ChannelPush:
		provider = s.d.Push.Name()
		providerID, err = s.d.Push.SendPush(ctx, m)
	case ChannelInApp:
		provider = s.d.InApp.Name()
		providerID, err = s.d.InApp.SendInApp(ctx, m)
	}
	if err != nil {
		return failure(provider, err)
	}
	sentAt := s.d.Clock.Now()
	return Result{Status: StatusSent, Provider: provider, ProviderMessageID: providerID, SentAt: &sentAt}, nil
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
	if s.d.Recipients == nil || m.Channel != ChannelEmail {
		return nil
	}
	return s.d.Recipients.Index(strings.ToLower(strings.TrimSpace(m.ToEmail)))
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
