package audit_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"uuid"

	"koro/internal/audit"
	"koro/internal/audit/audittest"
	"koro/internal/authz/authztest"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// recordingLogger is an audit.Logger that keeps what it was handed and
// fails like a PostgreSQL insert when the context has ended (or always,
// with fail set).
type recordingLogger struct {
	rows []audit.Event
	fail error
}

func (l *recordingLogger) Log(ctx context.Context, e audit.Event) error {
	l.rows = append(l.rows, e)
	if l.fail != nil {
		return l.fail
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// TestReauthFailed_OnlyVerdictsAreDenied (03 §6.2, BE-072, BE-074): the
// shared step every R module runs on a failed proof check. A verdict (a
// 4xx: missing or wrong proof 403, lockout 429) becomes the operation's
// record with outcome denied and reason reauth_failed — whatever the
// caller set there — and keeps its action, resource, org and actor; an
// error without a verdict (the request budget ran out, a dependency down,
// a bare store error) writes nothing. The error always comes back as it
// was, so httpx answers the same status.
func TestReauthFailed_OnlyVerdictsAreDenied(t *testing.T) {
	t.Parallel()
	required := problem.ErrForbidden.Wrap(errors.New("fake reauth: no proof"))
	wrong := problem.ErrForbidden.Wrap(errors.New("fake reauth: wrong proof"))
	cases := []struct {
		name    string
		err     error
		audited bool
	}{
		{"no proof 403", required, true},
		{"wrong proof 403", wrong, true},
		{"wrapped wrong proof 403", fmt.Errorf("payments: %w", wrong), true},
		{"locked out 429", problem.ErrRateLimited, true},
		{"request budget ran out", context.DeadlineExceeded, false},
		{"budget ran out in a store call", fmt.Errorf("identity: totp: %w", context.DeadlineExceeded), false},
		{"request cancelled", context.Canceled, false},
		{"limiter down 503", problem.ErrUnavailable.Wrap(errors.New("redis: connection refused")), false},
		{"store fault", fmt.Errorf("identity: user: %w", errors.New("driver: bad connection")), false},
		{"no error", nil, false},
	}
	user := authztest.ID("finance_admin")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l, logs := &recordingLogger{}, &bytes.Buffer{}
			res, org := uuid.NewV7(), uuid.NewV7()
			denied := audit.Event{
				Action: "ledger.payout.approve", Outcome: audit.Success, Reason: "set by mistake", OrgID: &org,
				ResourceType: "payout", ResourceID: &res, ActorUserID: &user, Metadata: map[string]any{"step": "approve"},
			}
			got := audit.ReauthFailed(t.Context(), l, obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelDebug}), denied, tc.err)
			if got != tc.err { //nolint:errorlint // the very same error must come back
				t.Fatalf("returned %v, want the error unchanged: %v", got, tc.err)
			}
			if logs.Len() != 0 {
				t.Fatalf("logged: %s", logs)
			}
			if !tc.audited {
				if len(l.rows) != 0 {
					t.Fatalf("an error without a verdict was audited: %+v", l.rows)
				}
				return
			}
			if len(l.rows) != 1 {
				t.Fatalf("%d rows for a refused proof, want 1", len(l.rows))
			}
			r := l.rows[0]
			switch {
			case r.Outcome != audit.Denied || r.Reason != audit.ReasonReauthFailed:
				t.Fatalf("outcome %q reason %q, want denied reauth_failed", r.Outcome, r.Reason)
			case r.Action != denied.Action || r.ResourceType != "payout" || r.ResourceID == nil || *r.ResourceID != res:
				t.Fatalf("resource lost: %+v", r)
			case r.OrgID == nil || *r.OrgID != org || r.ActorUserID == nil || *r.ActorUserID != user || r.Metadata["step"] != "approve":
				t.Fatalf("org, actor or step lost: %+v", r)
			}
		})
	}
}

// TestReauthFailed_UnwrittenRowLogLevel: a refused proof whose row cannot
// be written keeps its answer. On a request that has already ended (the
// verdict came at the deadline) that is an overload signal, WARN like
// httpx's 503; on a live request it is an audit fault, ERROR. A nil slog
// logger falls back to the default one instead of panicking.
func TestReauthFailed_UnwrittenRowLogLevel(t *testing.T) {
	t.Parallel()
	wrong := problem.ErrForbidden.Wrap(errors.New("fake reauth: wrong proof"))
	denied := audit.Event{Action: "catalog.policy.manage", ResourceType: "commerce_policy"}

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	logs := &bytes.Buffer{}
	err := audit.ReauthFailed(ended, &recordingLogger{}, obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelDebug}), denied, wrong)
	if !errors.Is(err, wrong) || !strings.Contains(logs.String(), `"level":"WARN","msg":"reauth failure not audited"`) ||
		strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("ended request: err %v, logs %s", err, logs)
	}

	logs.Reset()
	down := &recordingLogger{fail: errors.New("audit: insert: connection refused")}
	err = audit.ReauthFailed(t.Context(), down, obs.NewLogger(logs, obs.LoggerOptions{Level: slog.LevelDebug}), denied, wrong)
	if !errors.Is(err, wrong) || !strings.Contains(logs.String(), `"level":"ERROR","msg":"reauth failure not audited"`) ||
		!strings.Contains(logs.String(), `"action":"catalog.policy.manage"`) {
		t.Fatalf("live request: err %v, logs %s", err, logs)
	}

	if err := audit.ReauthFailed(t.Context(), down, nil, denied, wrong); !errors.Is(err, wrong) {
		t.Fatalf("nil slog logger: %v", err)
	}
}

// TestAuditTest_ValuesMatchAudit: audittest (which cannot import audit)
// compares the values audit writes.
func TestAuditTest_ValuesMatchAudit(t *testing.T) {
	t.Parallel()
	if audittest.Denied != string(audit.Denied) || audittest.Success != string(audit.Success) ||
		audittest.ReasonReauthFailed != audit.ReasonReauthFailed || audittest.MetaReauth != audit.MetaReauth {
		t.Fatal("audittest's values drifted from audit's")
	}
}
