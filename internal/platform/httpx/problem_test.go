package httpx_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx/op"
	"koro/internal/platform/problem"
)

var (
	errTestThingNotFound = problem.New(problem.NotFound, "testonly.thing_not_found", "Thing not found")
	errTestThingFull     = problem.New(problem.Conflict, "testonly.thing_full", "Thing is full")
	errTestThingInvalid  = problem.New(problem.Invalid, "testonly.thing_invalid", "Thing is invalid")
)

func TestProblem_UnknownError_Returns500WithoutRawText(t *testing.T) {
	t.Parallel()
	app := newTestApp(t, nil)
	registerGET(app.V1, "getBroken", "/broken", func(context.Context, *emptyInput) (*okOutput, error) {
		return nil, fmt.Errorf("load: %w", errors.New("pq: password authentication failed for user koro_app host 10.0.0.5"))
	})

	rec := app.do(t, http.MethodGet, "/api/v1/broken", nil, nil)
	m := problemBody(t, rec, http.StatusInternalServerError)
	if m["code"] != "internal_error" || m["title"] != "Internal server error" {
		t.Fatalf("unexpected problem %v", m)
	}
	if _, has := m["errors"]; has {
		t.Fatalf("500 must not carry errors[]: %v", m)
	}
	if _, has := m["detail"]; has {
		t.Fatalf("500 must not carry detail: %v", m)
	}
	for _, leak := range []string{"pq:", "10.0.0.5", "koro_app", "password authentication"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("response leaks %q: %s", leak, rec.Body.String())
		}
	}
	if logs := app.logs.String(); !strings.Contains(logs, "request failed") || !strings.Contains(logs, "10.0.0.5") {
		t.Fatalf("the raw error must be logged once for operators; logs = %s", logs)
	}
}

func TestProblem_WrappedSentinel_MapsToStatusAndCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantTitle  string
		wantFields int
	}{
		{"wrapped not found", fmt.Errorf("get thing: %w", errTestThingNotFound.Wrap(errors.New("sql: no rows in result set"))),
			http.StatusNotFound, "testonly.thing_not_found", "Thing not found", 0},
		{"double wrapped conflict", fmt.Errorf("svc: %w", fmt.Errorf("store: %w", errTestThingFull)),
			http.StatusConflict, "testonly.thing_full", "Thing is full", 0},
		{"invalid with fields", errTestThingInvalid.WithFields(problem.FieldError{Location: "body.name", Message: "must not be blank"}),
			http.StatusUnprocessableEntity, "testonly.thing_invalid", "Thing is invalid", 1},
		{"generic forbidden", problem.ErrForbidden, http.StatusForbidden, "forbidden", "Forbidden", 0},
		{"precondition required", problem.ErrPreconditionRequired, http.StatusPreconditionRequired, "precondition_required", "Precondition required", 0},
		{"internal sentinel", problem.ErrInternal.Wrap(errors.New("disk full at /var/lib/x")), http.StatusInternalServerError, "internal_error", "Internal server error", 0},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			app := newTestApp(t, nil)
			registerGET(app.V1, fmt.Sprintf("getSentinel%d", i), "/sentinel", func(context.Context, *emptyInput) (*okOutput, error) {
				return nil, tt.err
			})
			rec := app.do(t, http.MethodGet, "/api/v1/sentinel", nil, nil)
			m := problemBody(t, rec, tt.wantStatus)
			if m["code"] != tt.wantCode || m["title"] != tt.wantTitle {
				t.Fatalf("code/title = %v/%v, want %s/%s", m["code"], m["title"], tt.wantCode, tt.wantTitle)
			}
			errs, _ := m["errors"].([]any)
			if len(errs) != tt.wantFields {
				t.Fatalf("errors = %v, want %d entries", m["errors"], tt.wantFields)
			}
			for _, leak := range []string{"sql:", "/var/lib"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("response leaks the cause %q: %s", leak, rec.Body.String())
				}
			}
		})
	}
}

type signupInput struct {
	Body struct {
		Email    string `json:"email" format:"email" maxLength:"254"`
		Password string `json:"password" minLength:"12" maxLength:"128"`
	}
}

func newSignupApp(t *testing.T) *testApp {
	t.Helper()
	app := newTestApp(t, nil)
	huma.Register(app.V1, op.Koro(huma.Operation{
		OperationID: "createTestSignup", Method: http.MethodPost, Path: "/signups", Tags: []string{"testonly"},
	}, op.Meta{Action: "testonly.signup.create", Surface: op.SurfacePublic, Idempotency: op.IdempotencyOptional}),
		func(context.Context, *signupInput) (*okOutput, error) { return ok(), nil })
	return app
}

func TestProblem_ValidationError_HasNoValueField(t *testing.T) {
	t.Parallel()
	app := newSignupApp(t)
	tests := []struct {
		name  string
		body  string
		leaks []string
	}{
		{"invalid fields", `{"email":"not-an-email-addr","password":"short-pw1"}`, []string{"not-an-email-addr", "short-pw1"}},
		{"missing field echoes no body", `{"password":"hunter2-is-secret"}`, []string{"hunter2-is-secret"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := app.do(t, http.MethodPost, "/api/v1/signups", strings.NewReader(tt.body), nil)
			m := problemBody(t, rec, http.StatusUnprocessableEntity)
			if m["code"] != "validation_failed" {
				t.Fatalf("code = %v", m["code"])
			}
			errs, _ := m["errors"].([]any)
			if len(errs) == 0 {
				t.Fatalf("validation problem without errors[]: %v", m)
			}
			for _, e := range errs {
				fe, _ := e.(map[string]any)
				if _, has := fe["value"]; has {
					t.Fatalf("errors[] entry carries value: %v", fe)
				}
				if loc, _ := fe["location"].(string); !strings.HasPrefix(loc, "body") {
					t.Fatalf("errors[] entry without body location: %v", fe)
				}
			}
			for _, leak := range tt.leaks {
				if strings.Contains(rec.Body.String(), leak) {
					t.Fatalf("response echoes input %q: %s", leak, rec.Body.String())
				}
			}
		})
	}
}

func TestProblem_UnknownField_Returns422(t *testing.T) {
	t.Parallel()
	app := newSignupApp(t)
	rec := app.do(t, http.MethodPost, "/api/v1/signups",
		strings.NewReader(`{"email":"zeynep@example.test","password":"long-enough-password","is_admin":true}`), nil)
	m := problemBody(t, rec, http.StatusUnprocessableEntity)
	if m["code"] != "validation_failed" {
		t.Fatalf("code = %v", m["code"])
	}
	found := false
	errs, _ := m["errors"].([]any)
	for _, e := range errs {
		fe, _ := e.(map[string]any)
		if fe["location"] == "body.is_admin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unknown property is not reported at body.is_admin: %v", m["errors"])
	}

	okRec := app.do(t, http.MethodPost, "/api/v1/signups",
		strings.NewReader(`{"email":"zeynep@example.test","password":"long-enough-password"}`), nil)
	if okRec.Code != http.StatusOK {
		t.Fatalf("valid body status = %d; %s", okRec.Code, okRec.Body.String())
	}
}
