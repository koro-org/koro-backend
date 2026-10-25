package consent

import (
	"context"
	"errors"
	"log/slog"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/iys"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// iysWebhookResource is the authz view of an integrator's delivery.
var iysWebhookResource = authz.Resource{Type: authz.TypeIYSWebhook}

// ReceiveIYSWebhook implements Service (BE-110). The adapter checks the
// delivery before it stores anything; consent maps its answer: a wrong
// token or source address → 401, a body that is not the integrator's → 422,
// anything else (the store, identity) → 503 so that the integrator
// delivers again. No part of the delivery is logged.
func (s *service) ReceiveIYSWebhook(ctx context.Context, provider, token string, body []byte) error {
	if err := authz.Can(ctx, authz.ActorOrAnonymous(ctx), authz.ConsentIysWebhookReceive, iysWebhookResource); err != nil {
		return err
	}
	r := s.d.IYSPush
	if r == nil || provider != r.Name() {
		return problem.ErrNotFound
	}
	_, err := r.Receive(ctx, iys.Delivery{Token: token, ClientIP: httpx.ClientIPFrom(ctx), Body: body})
	if err == nil {
		return nil
	}
	reason, out := "unavailable", error(problem.ErrUnavailable)
	switch {
	case errors.Is(err, iys.ErrUnauthenticated):
		reason, out = "unauthenticated", problem.ErrUnauthenticated
	case errors.Is(err, iys.ErrRejected):
		reason, out = "malformed", problem.ErrValidation
	}
	s.d.Log.LogAttrs(ctx, slog.LevelWarn, "iys webhook rejected", slog.String("provider", provider),
		slog.String("reason", reason), slog.String("error", cut(obs.Scrub(err.Error()), maxLastError)))
	return out
}
