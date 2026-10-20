package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gin-gonic/gin"

	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// The huma error factories, gin's mode and huma's array nullability are
// package-level globals: they are installed exactly once per process
// (05 §10.1). Installing a different problem type base later is a
// programming error.
var (
	installOnce   sync.Once
	installedBase string
)

func installProblemFactory(base string) error {
	installOnce.Do(func() {
		installedBase = base
		gin.SetMode(gin.ReleaseMode)
		huma.DefaultArrayNullable = false
		// NewError has no request context (OpenAPI schema derivation, huma
		// helpers); nothing is logged there.
		huma.NewError = func(status int, msg string, errs ...error) huma.StatusError {
			p, _ := toProblem(nil, base, status, msg, errs)
			return p
		}
		huma.NewErrorWithContext = func(hctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
			ctx := hctx.Context()
			observeDenial(ctx, errs)
			p, f := toProblem(ctx.Err(), base, status, msg, errs)
			if f != nil {
				loggerFrom(ctx).LogAttrs(ctx, f.level, f.msg,
					slog.Int("status", p.Status), slog.String("code", p.Code), slog.Any("err", f.err))
			}
			p.Instance = instance(ctx)
			return p
		}
	})
	if installedBase != base {
		return fmt.Errorf("httpx: problem type base already installed as %q, cannot switch to %q", installedBase, base)
	}
	return nil
}

// toProblem is the single conversion point from errors to RFC 9457 bodies
// (05 §4.1, 01 R9):
//   - a *problem.Error anywhere in the chain wins (wrapped sentinels too);
//   - any other error with a 5xx status is a bare internal_error, its text
//     logged once and never sent, unless the request simply ran out of time
//     (requestEnded): then it is 503 unavailable, like the reply of Go's
//     http.TimeoutHandler (T-315, BE-071);
//   - huma's own 4xx errors keep their generic message and field locations,
//     never the submitted values.
//
// The second result is what the caller logs once (5xx only); it is nil
// otherwise. done is the Err() of the request context: nil while the
// request is live, or where huma has no request (NewError).
func toProblem(done error, base string, status int, msg string, errs []error) (*problem.Problem, *failure) {
	for _, err := range errs {
		var pe *problem.Error
		if errors.As(err, &pe) {
			p := problem.FromError(pe, base)
			if p.Status >= http.StatusInternalServerError {
				return p, newFailure(done, p.Status, err)
			}
			return p, nil
		}
	}
	if status >= http.StatusInternalServerError || status < http.StatusBadRequest {
		err := errors.Join(errs...)
		if requestEnded(done, err) != endNone {
			return problem.FromError(problem.ErrUnavailable, base), newFailure(done, http.StatusServiceUnavailable, err)
		}
		if status < http.StatusBadRequest {
			status = http.StatusInternalServerError
		}
		return problem.ForStatus(status, "", base), newFailure(done, status, err)
	}
	p := problem.ForStatus(status, msg, base)
	for _, err := range errs {
		if err == nil {
			continue
		}
		var d huma.ErrorDetailer
		if errors.As(err, &d) {
			ed := d.ErrorDetail()
			// ed.Value is dropped on purpose: huma echoes the input (passwords
			// included) there (R-002 §5.2-8).
			p.Errors = append(p.Errors, problem.FieldError{Location: ed.Location, Message: ed.Message})
			continue
		}
		p.Errors = append(p.Errors, problem.FieldError{Message: obs.Scrub(err.Error())})
	}
	return p, nil
}

// failure is the one log line of a 5xx response.
type failure struct {
	err   error
	level slog.Level
	msg   string
}

// Log messages of 5xx responses.
const (
	msgRequestFailed   = "request failed"           // ERROR: a server fault
	msgBudgetExhausted = "request budget exhausted" // WARN: overload, not a bug
	msgRequestCanceled = "request canceled"         // INFO: the caller left
)

// newFailure picks the log level of a 5xx response with status: a 503
// whose cause is the end of the request (requestEnded) is an overload
// signal (WARN) or a departed caller (INFO), never an ERROR alarm; every
// other 5xx logs at ERROR.
func newFailure(done error, status int, err error) *failure {
	if status == http.StatusServiceUnavailable {
		switch requestEnded(done, err) {
		case endBudget:
			return &failure{err: err, level: slog.LevelWarn, msg: msgBudgetExhausted}
		case endCanceled:
			return &failure{err: err, level: slog.LevelInfo, msg: msgRequestCanceled}
		}
	}
	return &failure{err: err, level: slog.LevelError, msg: msgRequestFailed}
}

// requestEnd says why a request stopped without a server fault.
type requestEnd uint8

const (
	endNone     requestEnd = iota
	endBudget              // the request budget or a dependency deadline ran out
	endCanceled            // the client went away or the server is shutting down
)

// sqlQueryCanceled is SQLSTATE 57014 (query_canceled): the server-side
// statement_timeout fired (05 §6.1, §10.6), or an operator cancelled the
// query. pgx itself never asks the server to cancel: an expiring request
// context surfaces as context.DeadlineExceeded or context.Canceled.
const sqlQueryCanceled = "57014"

// sqlStater is implemented by *pgconn.PgError; httpx does not import the
// driver.
type sqlStater interface {
	error
	SQLState() string
}

// requestEnded classifies err as the end of the request rather than a
// server fault (05 §10.6, T-315):
//   - context.DeadlineExceeded anywhere in the chain: the request budget
//     (requestTimeout) or a dependency's own deadline ran out;
//   - SQLSTATE 57014: the database gave up on the statement (overload);
//   - context.Canceled once the request context is done (done != nil):
//     the client disconnected or a hard shutdown cancelled the base
//     context. A Canceled error from a still-live request is a programming
//     error (a sub-context cancelled too early) and stays a server fault.
func requestEnded(done, err error) requestEnd {
	if err == nil {
		return endNone
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return endBudget
	}
	if s, ok := errors.AsType[sqlStater](err); ok && s.SQLState() == sqlQueryCanceled {
		return endBudget
	}
	if errors.Is(err, context.Canceled) && done != nil {
		if errors.Is(done, context.DeadlineExceeded) {
			return endBudget
		}
		return endCanceled
	}
	return endNone
}

func instance(ctx context.Context) string {
	if rid := obs.RequestID(ctx); rid != "" {
		return "urn:koro:request:" + rid
	}
	return ""
}

// respondProblem writes p as application/problem+json from a gin handler
// (router 404/405, recovery, CSRF rejection) and aborts the chain.
func respondProblem(c *gin.Context, p *problem.Problem) {
	p.Instance = instance(c.Request.Context())
	b, err := json.Marshal(p)
	if err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	c.Data(p.Status, problem.ContentType, b)
	c.Abort()
}

type loggerKey struct{}

func withLogger(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, log)
}

// loggerFrom returns the App logger stored by the first middleware; the
// error factories are process globals and need the per-App logger.
func loggerFrom(ctx context.Context) *slog.Logger {
	if ctx != nil {
		if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
			return l
		}
	}
	return obs.Discard()
}
