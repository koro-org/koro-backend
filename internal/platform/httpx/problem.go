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
			p, _ := toProblem(base, status, msg, errs)
			return p
		}
		huma.NewErrorWithContext = func(hctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
			ctx := hctx.Context()
			p, cause := toProblem(base, status, msg, errs)
			if cause != nil {
				loggerFrom(ctx).LogAttrs(ctx, slog.LevelError, "request failed",
					slog.Int("status", p.Status), slog.String("code", p.Code), slog.Any("err", cause))
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
// (05 §4.1):
//   - a *problem.Error anywhere in the chain wins (wrapped sentinels too);
//   - any other error with a 5xx status becomes a bare internal_error: its
//     text is logged once and never sent;
//   - huma's own 4xx errors keep their generic message and field locations,
//     never the submitted values.
//
// The second result is the error the caller must log once at ERROR level
// (5xx only); it is nil otherwise.
func toProblem(base string, status int, msg string, errs []error) (*problem.Problem, error) {
	for _, err := range errs {
		var pe *problem.Error
		if errors.As(err, &pe) {
			if pe.Status() >= http.StatusInternalServerError {
				return problem.FromError(pe, base), err
			}
			return problem.FromError(pe, base), nil
		}
	}
	if status >= http.StatusInternalServerError || status < http.StatusBadRequest {
		if status < http.StatusBadRequest {
			status = http.StatusInternalServerError
		}
		return problem.ForStatus(status, "", base), errors.Join(errs...)
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
