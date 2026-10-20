// Package audittest helps module tests check what an R operation hands to
// audit when its re-authentication step fails (03 §6.2, BE-074): a refused
// proof is one denied reauth_failed record, an overloaded server none.
//
// It is imported only by tests (module service tests). It does not import
// audit (internal/arch: a package of internal/audit may import only lower
// layers); a test maps its recorded audit.Event values to Row.
package audittest

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
)

// The values of audit the checks compare (audit.Denied, audit.Success,
// audit.ReasonReauthFailed, audit.MetaReauth; audit's tests pin them).
const (
	Denied             = "denied"
	Success            = "success"
	ReasonReauthFailed = "reauth_failed"
	MetaReauth         = "reauth"
)

// Row is the checked part of an audit.Event.
type Row struct {
	Action       string
	Outcome      string
	Reason       string
	ActorUserID  *uuid.UUID
	ActorOrgID   *uuid.UUID
	SystemJob    string
	OrgID        *uuid.UUID
	ResourceType string
	ResourceID   *uuid.UUID
	Metadata     map[string]any
}

// Verifier is a module's re-authentication port (identity's Verify).
type Verifier[P any] interface {
	Verify(ctx context.Context, userID uuid.UUID, proof P) (kind string, err error)
}

// LateReauth wraps a module's test verifier. While late is set it answers
// as identity does on an overloaded server: only once the request context
// has ended, with the bare context error (BE-070; identity's
// TestPasswordPaths_PastRequestDeadline_503NoLockout pins that answer).
// Use it inside a synctest bubble (Overloaded).
type LateReauth[P any] struct {
	Next Verifier[P]
	late atomic.Bool
}

// Verify implements the module's Reauthenticator.
func (r *LateReauth[P]) Verify(ctx context.Context, userID uuid.UUID, proof P) (string, error) {
	if r.late.Load() {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return r.Next.Verify(ctx, userID, proof)
}

// SetLate switches the overload on or off.
func (r *LateReauth[P]) SetLate(on bool) { r.late.Store(on) }

// ReauthCase is one R operation under test: Call runs it with a proof in a
// fresh request context, Rows returns the audit rows of its action written
// so far, Denied is the record a refused proof must produce (action,
// actor, resource type and ID, org, metadata; outcome and reason are
// checked apart).
type ReauthCase[P any] struct {
	Call   func(ctx context.Context, proof P) error
	Rows   func() []Row
	Denied Row
	Late   *LateReauth[P]

	// Wrong and Missing are refused by the module's test verifier with
	// WrongErr (identity.reauth_failed) and MissingErr
	// (identity.reauth_required); Good passes.
	Wrong, Missing, Good P
	WrongErr, MissingErr error
}

// Run checks c (03 §6.2, BE-072, BE-074):
//
//   - a wrong and a missing proof are refused unchanged (the module's 403)
//     and each writes exactly one denied reauth_failed record (c.Denied);
//   - on an overloaded server (identity cannot answer within the request
//     budget) the call fails with the bare deadline after the whole budget
//     (httpx answers 503) and tries no record at all;
//   - the right proof afterwards goes through with one success record that
//     names the proof's kind: the refusals and the overload changed nothing.
func (c ReauthCase[P]) Run(t *testing.T) {
	t.Helper()
	for _, refused := range []struct {
		name  string
		proof P
		want  error
	}{{"wrong proof", c.Wrong, c.WrongErr}, {"missing proof", c.Missing, c.MissingErr}} {
		n := len(c.Rows())
		if err := c.Call(t.Context(), refused.proof); !errors.Is(err, refused.want) {
			t.Fatalf("%s %s: err %v, want %v", c.Denied.Action, refused.name, err, refused.want)
		}
		WantDenied(t, c.Rows()[n:], c.Denied)
	}

	n := len(c.Rows())
	c.Late.SetLate(true)
	Overloaded(t, func(ctx context.Context) error { return c.Call(ctx, c.Good) })
	c.Late.SetLate(false)
	if rows := c.Rows()[n:]; len(rows) != 0 {
		t.Fatalf("%s: %d audit rows tried on an overload; it is no verdict on the proof: %+v", c.Denied.Action, len(rows), rows)
	}

	if err := c.Call(t.Context(), c.Good); err != nil {
		t.Fatalf("%s with the right proof after the refusals: %v", c.Denied.Action, err)
	}
	rows := c.Rows()[n:]
	if len(rows) != 1 || rows[0].Outcome != Success || rows[0].Metadata[MetaReauth] == nil {
		t.Fatalf("%s success record = %+v", c.Denied.Action, rows)
	}
}

// WantDenied asserts that rows (what one refused call wrote for its action)
// are exactly one denied reauth_failed record matching want: action,
// actor, resource type and ID, org and metadata (nil and empty alike).
func WantDenied(t testing.TB, rows []Row, want Row) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("%s: %d audit rows for a refused proof, want one denied record (03 §6.2): %+v", want.Action, len(rows), rows)
	}
	r := rows[0]
	switch {
	case r.Outcome != Denied || r.Reason != ReasonReauthFailed:
		t.Fatalf("%s: outcome %q reason %q, want denied reauth_failed", want.Action, r.Outcome, r.Reason)
	case r.Action != want.Action || r.ResourceType != want.ResourceType:
		t.Fatalf("denied record %s %s, want %s %s", r.Action, r.ResourceType, want.Action, want.ResourceType)
	case !sameID(r.ActorUserID, want.ActorUserID) || r.SystemJob != "":
		t.Fatalf("%s: actor %v, want %v", want.Action, r.ActorUserID, want.ActorUserID)
	case !sameID(r.ResourceID, want.ResourceID):
		t.Fatalf("%s: resource ID %v, want %v", want.Action, r.ResourceID, want.ResourceID)
	case !sameID(r.OrgID, want.OrgID) || !sameID(r.ActorOrgID, want.ActorOrgID):
		t.Fatalf("%s: org %v (actor org %v), want %v (%v)", want.Action, r.OrgID, r.ActorOrgID, want.OrgID, want.ActorOrgID)
	case !sameMeta(r.Metadata, want.Metadata):
		t.Fatalf("%s: metadata %v, want %v (nothing happened: no business data)", want.Action, r.Metadata, want.Metadata)
	}
}

// Overloaded runs call inside a synctest bubble with the production request
// budget and asserts the overload answer: the request deadline itself after
// the whole budget, inside no *problem.Error (one would take precedence in
// httpx, T-315), so that httpx answers 503 unavailable.
func Overloaded(t *testing.T, call func(ctx context.Context) error) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), httpx.DefaultRequestTimeout)
		defer cancel()
		start := time.Now() //nolint:forbidigo // inside the synctest bubble time.Now is the fake clock
		err := call(ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("overloaded call = %v, want the request deadline", err)
		}
		if pe, ok := errors.AsType[*problem.Error](err); ok {
			t.Fatalf("overload became problem %q; httpx maps the bare deadline to 503", pe.Code())
		}
		if waited := time.Since(start); waited != httpx.DefaultRequestTimeout {
			t.Fatalf("answered after %v, want the whole budget %v", waited, httpx.DefaultRequestTimeout)
		}
	})
}

func sameID(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameMeta(a, b map[string]any) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	return reflect.DeepEqual(a, b)
}
