package httpx_test

import (
	"errors"
	"net/http"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
)

func TestETag_StrongQuotedBase32Nanos(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 25, 9, 0, 0, 123, time.UTC)
	if got, want := httpx.ETag(at), `"1hodq4osht83r"`; got != want {
		// The literal pins the encoding (base 32 of UnixNano): every module's
		// tags and every client's stored If-Match depend on it.
		t.Fatalf("ETag = %s, want %s", got, want)
	}
	if httpx.ETag(at) == httpx.ETag(at.Add(time.Nanosecond)) {
		t.Fatal("ETag does not change with updated_at")
	}
	if httpx.ETag(at) != httpx.ETag(at.In(time.FixedZone("TRT", 3*3600))) {
		t.Fatal("ETag depends on the time zone")
	}
}

func TestCheckIfMatch_428And412(t *testing.T) {
	t.Parallel()
	cur := `"abc"`
	for _, tc := range []struct {
		header string
		want   error
		status int
	}{
		{"", problem.ErrPreconditionRequired, http.StatusPreconditionRequired},
		{"   ", problem.ErrPreconditionRequired, http.StatusPreconditionRequired},
		{"*", problem.ErrPreconditionRequired, http.StatusPreconditionRequired},
		{` * `, problem.ErrPreconditionRequired, http.StatusPreconditionRequired},
		{`"old"`, problem.ErrPreconditionFailed, http.StatusPreconditionFailed},
		{`W/"abc"`, problem.ErrPreconditionFailed, http.StatusPreconditionFailed},
		{`"abc"`, nil, 0},
		{` "old" , "abc" `, nil, 0},
	} {
		err := httpx.CheckIfMatch(tc.header, cur)
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Fatalf("CheckIfMatch(%q) = %v, want %v", tc.header, err, tc.want)
		}
		if p, ok := errors.AsType[*problem.Error](err); ok && p.Status() != tc.status {
			t.Fatalf("CheckIfMatch(%q) status = %d, want %d", tc.header, p.Status(), tc.status)
		}
	}
}

func TestParseID_MalformedIsNil(t *testing.T) {
	t.Parallel()
	id := uuid.NewV7()
	if got := httpx.ParseID(id.String()); got != id {
		t.Fatalf("ParseID = %s, want %s", got, id)
	}
	for _, s := range []string{"", "not-a-uuid", id.String() + "x"} {
		if got := httpx.ParseID(s); got != uuid.Nil() {
			t.Fatalf("ParseID(%q) = %s, want nil UUID (404 path)", s, got)
		}
	}
}

func TestOptID_EmptyNilMalformed422(t *testing.T) {
	t.Parallel()
	got, err := httpx.OptID("query.org_id", "")
	if got != nil || err != nil {
		t.Fatalf("OptID(\"\") = %v, %v", got, err)
	}
	id := uuid.NewV7()
	got, err = httpx.OptID("query.org_id", id.String())
	if err != nil || got == nil || *got != id {
		t.Fatalf("OptID(id) = %v, %v", got, err)
	}
	_, err = httpx.OptID("query.org_id", "x")
	p, ok := errors.AsType[*problem.Error](err)
	if !ok || p.Status() != http.StatusUnprocessableEntity || p.Code() != "validation_failed" {
		t.Fatalf("OptID(malformed) = %v, want 422 validation_failed", err)
	}
	if f := p.Fields(); len(f) != 1 || f[0].Location != "query.org_id" || f[0].Message != "must be a UUID" {
		t.Fatalf("fields = %+v", f)
	}
}

func TestIDStr(t *testing.T) {
	t.Parallel()
	if httpx.IDStr(nil) != "" {
		t.Fatal("IDStr(nil) is not empty")
	}
	id := uuid.NewV7()
	if httpx.IDStr(&id) != id.String() {
		t.Fatal("IDStr(&id) is not id.String()")
	}
}
