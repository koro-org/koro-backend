package messaging_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"koro/internal/authz/authztest"
	"koro/internal/messaging"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/httpxtest"
	"koro/internal/platform/obs"
)

// TestHTTP_ReportMyMessage_RepeatConformsToSchema (BE-169, WEB-056): the
// student reports a company message over HTTP: 201 with the new report,
// then the same report again: 200 with the same report_id. Both answers
// match the schema the served OpenAPI document gives their status (the
// 200 was undocumented), as does the company side's operation.
func TestHTTP_ReportMyMessage_RepeatConformsToSchema(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, c := e.accepted(t, "student")
	conv := c.Conversation.ID
	msgs := e.thread(t, conv)
	app, err := httpx.New(httpx.Deps{Log: obs.Discard(), Actors: authztest.NewResolver(append(authztest.Personas(), authztest.Helpers()...)...)})
	if err != nil {
		t.Fatal(err)
	}
	messaging.Register(app.V1, e.svc)

	path := "/api/v1/me/conversations/" + conv.String() + "/messages/" + msgs[6].ID.String() + "/report"
	var first string
	for i, want := range []int{http.StatusCreated, http.StatusOK} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, bytes.NewBufferString(`{"reason":"harassment"}`))
		req.Header.Set("Content-Type", "application/json")
		p, _ := authztest.Get("student")
		req.Header.Set("Authorization", "Bearer "+p.Token)
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("report %d: status = %d, want %d: %s", i+1, rec.Code, want, rec.Body)
		}
		httpxtest.WantConforms(t, app.API, "reportMyConversationMessage", rec.Code, rec.Body.Bytes())
		if i == 0 {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Fatalf("repeat answered %s, want the first report %s", rec.Body, first)
		}
	}
	// The org side documents the same 200 (its own call is covered by the
	// service tests).
	httpxtest.WantConforms(t, app.API, "reportOrgConversationMessage", http.StatusOK, []byte(first))
}
