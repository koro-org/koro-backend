package notify

import (
	"errors"

	"koro/internal/platform/problem"
)

// Errors of the notify module (05 §4.2). Send runs in the worker, so these
// reach the worker handler, not an HTTP client; the handler decides between
// retry and give-up with IsPermanent.
var (
	// ErrInvalidMessage: the message, its template or its attachments are
	// wrong. Field errors name the offending member, never its value.
	// Permanent: retrying the same message cannot help.
	ErrInvalidMessage = problem.New(problem.Invalid, "notify.invalid_message", "Notification message is invalid")

	// ErrCommercialDisabled: a commercial message was sent while no consent
	// checker is wired. Commercial messages are rejected, never sent
	// unchecked (safe default). Permanent.
	ErrCommercialDisabled = problem.New(problem.Forbidden, "notify.commercial_disabled",
		"Commercial notifications are disabled until consent checking is wired")

	// ErrNotInWorker: Send was called with a request context. Side effects
	// run in the worker (01 §0.5); a handler must enqueue instead. Permanent.
	ErrNotInWorker = problem.New(problem.Internal, "notify.not_in_worker",
		"Notifications can only be sent from a worker context")

	// ErrDeliveryInFlight: another attempt of the same dedupe key holds the
	// delivery. Retryable: by the next try that attempt has finished or its
	// lease has expired.
	ErrDeliveryInFlight = problem.New(problem.Conflict, "notify.delivery_in_flight",
		"Another attempt of this notification is in progress")

	// ErrDeliveryFailed: the provider could not take the message now
	// (network, timeout, 4xx). Retryable.
	ErrDeliveryFailed = problem.New(problem.Unavailable, "notify.delivery_failed", "Notification delivery failed")

	// ErrDeliveryRejected: the provider refused the recipient or the content
	// for good (SMTP 5xx on RCPT/DATA). Permanent.
	ErrDeliveryRejected = problem.New(problem.Invalid, "notify.delivery_rejected", "Notification was rejected by the provider")

	// ErrNotFound: the delivery does not exist (store contract).
	ErrNotFound = problem.New(problem.NotFound, "notify.not_found", "Notification delivery not found")

	// ErrStoreNotWired: the delivery store is not wired yet (the GORM store
	// arrives with BE-015). Never a silent in-memory fallback (05 §12.2).
	ErrStoreNotWired = problem.New(problem.Unavailable, "notify.store_not_wired", "Notification delivery store is not wired")
)

// permanentErrors are the errors a retry cannot fix.
var permanentErrors = []*problem.Error{ErrInvalidMessage, ErrCommercialDisabled, ErrNotInWorker, ErrDeliveryRejected}

// IsPermanent reports whether err is a Send failure that a retry cannot fix.
// A worker handler wraps such errors with asynq.SkipRetry (05 §8.4-3); every
// other error (delivery failed or in flight, store or context errors) is
// transient and retried with backoff.
func IsPermanent(err error) bool {
	for _, p := range permanentErrors {
		if errors.Is(err, p) {
			return true
		}
	}
	return false
}
