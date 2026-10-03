package testutil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Backoff is the retry policy of the environment setup (06 H-36): under
// parallel Docker load the first connections to a fresh container may fail
// with "unexpected EOF" or "connection reset", and container start-up (or
// the reaper) may time out. Attempt n (1-based) waits
// min(Base·2^(n-1), Max) plus up to 50 % jitter before the next try.
type Backoff struct {
	Attempts int           // total tries, at least 1
	Base     time.Duration // first delay
	Max      time.Duration // cap of one delay
}

// setupBackoff is used for container start-up, template preparation and
// per-test connections. Six tries span about 15 s before jitter.
var setupBackoff = Backoff{Attempts: 6, Base: 250 * time.Millisecond, Max: 8 * time.Second}

// Delay returns the wait after failed attempt n (1-based), without jitter.
func (b Backoff) Delay(n int) time.Duration {
	d := b.Base
	for i := 1; i < n && d < b.Max; i++ {
		d *= 2
	}
	return min(d, b.Max)
}

// Retry runs fn until it succeeds, returns an error retry does not accept,
// the attempts are used up or ctx ends. retryable nil means "every error".
// The last error is returned, annotated with what and the attempt count.
func Retry(ctx context.Context, b Backoff, what string, retryable func(error) bool, fn func(ctx context.Context) error) error {
	attempts := max(b.Attempts, 1)
	var err error
	for n := 1; ; n++ {
		if err = fn(ctx); err == nil {
			return nil
		}
		if n == attempts || (retryable != nil && !retryable(err)) {
			break
		}
		d := b.Delay(n)
		d += rand.N(d/2 + 1) //nolint:gosec // retry jitter, not security relevant
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("testutil: %s: %w (after %d attempts; last error: %w)", what, ctx.Err(), n, err)
		case <-t.C:
		}
	}
	return fmt.Errorf("testutil: %s: %w", what, err)
}

// Transient reports whether err is a connection-level failure worth a retry:
// a dropped or refused connection, a timeout, or a server that is starting,
// restarting or out of connection slots. Statement errors (syntax,
// permission, constraint) are not transient.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if _, ok := errors.AsType[*pgconn.ConnectError](err); ok {
		return true
	}
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch {
		case len(pe.Code) == 5 && pe.Code[:2] == "08": // connection exception class
			return true
		case pe.Code == "57P01", pe.Code == "57P02", pe.Code == "57P03", pe.Code == "53300":
			// admin/crash shutdown, cannot connect now (starting up), too many connections
			return true
		}
		return false
	}
	if ne, ok := errors.AsType[net.Error](err); ok && ne.Timeout() {
		return true
	}
	return pgconn.SafeToRetry(err)
}
