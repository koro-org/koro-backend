package audit_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
)

// A password, a token, a key or a special-category value (KVKK md. 6) never
// reaches the log: by key name (anywhere in the tree) or by value shape.
// Nothing is stored and the error names the key, never the value.
func TestLogger_RejectsSecretMetadata(t *testing.T) {
	t.Parallel()
	const secret = "Sup3rS3cret-Value"
	rejected := map[string]map[string]any{
		"password":                {"password": secret},
		"suffix _password":        {"new_password": secret},
		"reset token":             {"reset_token": secret},
		"client secret":           {"client_secret": secret},
		"api key":                 {"api_key": secret},
		"totp code":               {"totp_code": "123456"},
		"recovery codes":          {"recovery_codes": []string{secret}},
		"authorization header":    {"Authorization": "Basic " + secret},
		"nested credential":       {"changes": map[string]any{"session_token": secret}},
		"credential in an array":  {"items": []any{map[string]any{"pwd": secret}}},
		"health":                  {"health": "diabetes " + secret},
		"religion":                {"religion": secret},
		"sexual orientation":      {"profile": map[string]any{"sexual_orientation": secret}},
		"criminal record":         {"criminal_record": secret},
		"biometric data":          {"biometric_data": secret},
		"union membership":        {"union_membership": secret},
		"bearer token value":      {"note": "Bearer " + secret + "abcdef"},
		"jwt value":               {"note": "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0." + secret},
		"key=value credential":    {"query": "a=1&token=" + secret},
		"private key value":       {"pem": "-----BEGIN PRIVATE KEY-----\n" + secret},
		"password hash value":     {"stored": "$argon2id$v=19$m=65536,t=3,p=4$" + secret},
		"session cookie value":    {"cookie_header": "__Host-koro_session=" + secret},
		"dashed credential key":   {"x-api-key": secret},
		"upper-case credential":   {"PASSWORD": secret},
		"special category suffix": {"student_ethnicity": secret},
	}
	for name, meta := range rejected {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, nil)
			err := e.svc.Log(t.Context(), audit.Event{Action: "identity.user.update", Outcome: audit.Success, Metadata: meta})
			if !errors.Is(err, audit.ErrSecretMetadata) {
				t.Fatalf("Log() error = %v, want ErrSecretMetadata", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaks the value: %v", err)
			}
			if n := e.chain(t); n != 0 {
				t.Fatalf("%d rows stored, want none", n)
			}
		})
	}

	// References to credentials, the reauth kind and flags are fine.
	allowed := []map[string]any{
		{"reauth": "password"},
		{"reauth": "totp", "token_id": uuid.NewV7().String(), "field": "email"},
		{"break_glass": true, "subject_user_id": uuid.NewV7().String(), "ticket_ref": "SUP-1042"},
		{"sensitive": true, "member_count": 12, "fields": []string{"display_name", "bio"}},
		{"address_changed": true, "trace_id": "abc"}, // "address"/"trace" only look like dress/race
	}
	e := newEnv(t, nil)
	for _, meta := range allowed {
		if err := e.svc.Log(t.Context(), audit.Event{Action: "identity.user.update", Outcome: audit.Success, Metadata: meta}); err != nil {
			t.Errorf("Log(%v) error = %v", meta, err)
		}
	}
}

func TestLogger_ValidatesEvent(t *testing.T) {
	t.Parallel()
	user := uuid.NewV7()
	bad := map[string]audit.Event{
		"action format":         {Action: "events.Create", Outcome: audit.Success},
		"single segment":        {Action: "events", Outcome: audit.Success},
		"outcome":               {Action: "events.event.create", Outcome: "ok"},
		"user and system":       {Action: "events.event.create", Outcome: audit.Success, ActorUserID: &user, SystemJob: "x.y"},
		"reason too long":       {Action: "events.event.create", Outcome: audit.Success, Reason: strings.Repeat("ş", 501)},
		"resource without type": {Action: "events.event.create", Outcome: audit.Success, ResourceID: &user},
		"break_glass type":      {Action: "students.profile.read_admin", Outcome: audit.Success, Metadata: map[string]any{"break_glass": "yes"}},
		"subject not a uuid":    {Action: "students.profile.read_admin", Outcome: audit.Success, Metadata: map[string]any{"subject_user_id": "me"}},
		"reauth kind":           {Action: "identity.user.update", Outcome: audit.Success, Metadata: map[string]any{"reauth": "sms"}},
		"metadata too large":    {Action: "identity.user.update", Outcome: audit.Success, Metadata: map[string]any{"x": strings.Repeat("a", 5000)}},
		"metadata too deep": {Action: "identity.user.update", Outcome: audit.Success,
			Metadata: map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": 1}}}}},
	}
	for name, ev := range bad {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := newEnv(t, nil).svc.Log(t.Context(), ev); !errors.Is(err, audit.ErrInvalidEvent) {
				t.Fatalf("Log() error = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

// IP, User-Agent and request ID come from the context; the actor kind from
// the event (01 §4.1).
func TestLogger_ContextFieldsAndActorKind(t *testing.T) {
	t.Parallel()
	e := newEnv(t, nil)
	ctx := obs.WithRequestID(httpx.WithUserAgent(httpx.WithClientIP(t.Context(), "2001:DB8::1"), "Mozilla/5.0 Chrome/140"), "req-0001-abcd")
	a := persona(t, "student")
	for _, ev := range []audit.Event{
		audit.Event{Action: "identity.session.create", Outcome: audit.Success}.By(a),
		audit.Event{Action: "audit.chain.verify", Outcome: audit.Success}.By(authz.SystemActor("audit.verify_chain")),
		{Action: "identity.session.create", Outcome: audit.Denied, Reason: "bad_password"},
	} {
		if err := e.svc.Log(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	e.chain(t)
	rows := e.chained(t)
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	r := rows[0]
	if r.IP != "2001:db8::1" || r.UserAgent != "Mozilla/5.0 Chrome/140" || r.RequestID != "req-0001-abcd" ||
		r.ActorKind != "user" || *r.ActorUserID != a.UserID || !r.OccurredAt.Equal(t0) || string(r.Metadata) != "{}" {
		t.Fatalf("row = %+v", r)
	}
	if rows[1].ActorKind != "system" || rows[1].SystemJob != "audit.verify_chain" || rows[1].ActorUserID != nil {
		t.Fatalf("system row = %+v", rows[1])
	}
	if rows[2].ActorKind != "anonymous" || rows[2].Reason != "bad_password" || rows[2].Outcome != audit.Denied {
		t.Fatalf("anonymous row = %+v", rows[2])
	}
}

// CanonicalJSON is the hashed form of a row: a format change breaks every
// stored chain, so it is pinned byte for byte.
func TestCanonicalJSON_Golden(t *testing.T) {
	t.Parallel()
	user := uuid.MustParse("01920000-0000-7000-8000-000000000001")
	org := uuid.MustParse("01920000-0000-7000-8000-0000000000aa")
	r := audit.Record{
		ID: uuid.MustParse("01920000-0000-7000-8000-0000000000ff"), Seq: 42,
		OccurredAt: time.Date(2026, 10, 3, 12, 0, 0, 123456000, time.FixedZone("TRT", 3*3600)),
		ActorKind:  "user", ActorUserID: &user, Action: "orgs.member.list", Outcome: audit.Success, OrgID: &org,
		ResourceType: "org", ResourceID: &org, IP: "192.0.2.7", UserAgent: "Koro/1.0 <iOS>",
		Metadata: json.RawMessage(`{"z": 1.50, "a": {"y": true, "b": null}, "sensitive": true}`),
	}
	got, err := audit.CanonicalJSON(r)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"action":"orgs.member.list","actor_kind":"user","actor_org_id":null,` +
		`"actor_user_id":"01920000-0000-7000-8000-000000000001","id":"01920000-0000-7000-8000-0000000000ff",` +
		`"ip":"192.0.2.7","metadata":{"a":{"b":null,"y":true},"sensitive":true,"z":1.50},` +
		`"occurred_at":"2026-10-03T09:00:00.123456Z","org_id":"01920000-0000-7000-8000-0000000000aa",` +
		`"outcome":"success","reason":null,"request_id":null,"resource_id":"01920000-0000-7000-8000-0000000000aa",` +
		`"resource_type":"org","seq":42,"system_job":null,"user_agent":"Koro/1.0 <iOS>"}`
	if string(got) != want {
		t.Fatalf("CanonicalJSON =\n%s\nwant\n%s", got, want)
	}
	h1, _ := audit.RowHash(make([]byte, audit.HashSize), r)
	r.Reason = "x"
	h2, _ := audit.RowHash(make([]byte, audit.HashSize), r)
	if len(h1) != audit.HashSize || string(h1) == string(h2) {
		t.Fatal("RowHash must cover every column")
	}
}
