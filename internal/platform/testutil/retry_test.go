package testutil_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"koro/internal/platform/testutil"
)

var fast = testutil.Backoff{Attempts: 4, Base: time.Millisecond, Max: 4 * time.Millisecond}

func TestBackoff_DelayDoublesUpToMax(t *testing.T) {
	t.Parallel()
	b := testutil.Backoff{Attempts: 8, Base: 250 * time.Millisecond, Max: 2 * time.Second}
	want := []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w {
			t.Errorf("Delay(%d) = %s, want %s", i+1, got, w)
		}
	}
}

// The setup failure of 06 H-36: the first connections drop with
// "unexpected EOF", then the server answers.
func TestRetry_RecoversFromTransientConnectionErrors(t *testing.T) {
	t.Parallel()
	calls := 0
	err := testutil.Retry(t.Context(), fast, "connect", testutil.Transient, func(context.Context) error {
		calls++
		if calls < 3 {
			return fmt.Errorf("failed to connect: %w", io.ErrUnexpectedEOF)
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("Retry() = %v after %d calls, want success on the third", err, calls)
	}
}

func TestRetry_StopsOnPermanentErrorAndAfterAttempts(t *testing.T) {
	t.Parallel()
	perm := &pgconn.PgError{Code: "42501", Message: "permission denied"}
	calls := 0
	err := testutil.Retry(t.Context(), fast, "create database", testutil.Transient, func(context.Context) error {
		calls++
		return perm
	})
	if calls != 1 || !errors.Is(err, perm) {
		t.Fatalf("permanent error: %d calls, err %v; want one call and the error", calls, err)
	}

	calls = 0
	err = testutil.Retry(t.Context(), fast, "start", nil, func(context.Context) error {
		calls++
		return syscall.ECONNRESET
	})
	if calls != fast.Attempts || !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("exhausted: %d calls, err %v; want %d calls and the last error", calls, err, fast.Attempts)
	}
}

func TestRetry_HonoursContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	slow := testutil.Backoff{Attempts: 5, Base: time.Hour, Max: time.Hour}
	calls := 0
	err := testutil.Retry(ctx, slow, "start", nil, func(context.Context) error {
		calls++
		cancel()
		return io.EOF
	})
	if calls != 1 || !errors.Is(err, context.Canceled) || !errors.Is(err, io.EOF) {
		t.Fatalf("Retry() = %v after %d calls, want the cancellation and the last error", err, calls)
	}
}

func TestTransient_Classification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{io.ErrUnexpectedEOF, true},
		{fmt.Errorf("read: %w", syscall.ECONNRESET), true},
		{&pgconn.PgError{Code: "57P03"}, true},  // the database system is starting up
		{&pgconn.PgError{Code: "08006"}, true},  // connection failure
		{&pgconn.PgError{Code: "53300"}, true},  // too many connections
		{&pgconn.PgError{Code: "42P04"}, false}, // duplicate database
		{errors.New("syntax"), false},
		{nil, false},
	} {
		if got := testutil.Transient(tc.err); got != tc.want {
			t.Errorf("Transient(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
