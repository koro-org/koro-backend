package events_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"koro/internal/events"
	"koro/internal/platform/problem"
)

// answersForm has one field of every type (R-003 §2.5.1).
const answersForm = `{"schema_version":1,"fields":[
 {"key":"motivation","type":"long_text","label":{"tr":"Neden katılmak istiyorsun?"},"required":true,"validation":{"max_length":20}},
 {"key":"nickname","type":"short_text","label":{"tr":"Takma ad"},"required":false,"validation":{"pattern":"^[a-z]+$","max_length":10}},
 {"key":"tshirt","type":"single_select","label":{"tr":"Beden"},"required":false,
  "options":[{"value":"s","label":{"tr":"S"}},{"value":"m","label":{"tr":"M"}},{"value":"xl","label":{"tr":"XL"},"deleted":true}]},
 {"key":"topics","type":"multi_select","label":{"tr":"Konular"},"required":false,
  "options":[{"value":"go","label":{"tr":"Go"}},{"value":"sql","label":{"tr":"SQL"}},{"value":"web","label":{"tr":"Web"}}],
  "validation":{"max_select":2}},
 {"key":"rules","type":"checkbox","label":{"tr":"Etkinlik kurallarını okudum"},"required":true},
 {"key":"age","type":"number","label":{"tr":"Yaş"},"required":false,"validation":{"integer":true,"min":16,"max":99}},
 {"key":"portfolio","type":"url","label":{"tr":"Portfolyo"},"required":false,"validation":{"allowed_hosts":["github.com"]}},
 {"key":"phone","type":"phone","label":{"tr":"Telefon"},"required":false},
 {"key":"day","type":"date","label":{"tr":"Uygun gün"},"required":false,"validation":{"min_date":"2026-11-01","max_date":"2026-11-30"}},
 {"key":"share_sponsor","type":"consent","required":false,"consent_purpose":"event_sponsor_share"}
]}`

func mustSchema(t testing.TB, raw string) events.FormSchema {
	t.Helper()
	s, err := events.ParseFormSchema([]byte(raw), "body.draft_schema")
	if err != nil {
		t.Fatalf("ParseFormSchema: %v (%v)", err, fieldsOf(err))
	}
	return s
}

func fieldsOf(err error) []problem.FieldError {
	if p, ok := errors.AsType[*problem.Error](err); ok {
		return p.Fields()
	}
	return nil
}

func decodeAnswers(t testing.TB, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("answers %s: %v", raw, err)
	}
	return m
}

// TestValidateAnswers_Table: the server-side answer check (ADR-0013): typed
// answers (a number is never a string), required fields, lengths, options
// (a deleted option is no longer an option), unknown fields, and the rules
// JSON Schema cannot express (phone, URL host, date range).
func TestValidateAnswers_Table(t *testing.T) {
	t.Parallel()
	s := mustSchema(t, answersForm)
	const base = `"motivation":"Go öğrenmek","rules":true`
	tests := []struct {
		name    string
		answers string
		errs    map[string]string // location → message (substring); empty: valid
		want    map[string]any    // cleaned answers of a valid case (nil: not checked)
	}{
		{name: "all fields, cleaned", answers: `{"motivation":"  Go\r\nöğrenmek  ","nickname":" ada ","tshirt":"m",
			"topics":["go","sql"],"rules":true,"age":20,"portfolio":"https://github.com/ada","phone":"0532 123 45 67",
			"day":"2026-11-15","share_sponsor":false}`,
			want: map[string]any{"motivation": "Go\nöğrenmek", "nickname": "ada", "tshirt": "m", "topics": []any{"go", "sql"},
				"rules": true, "age": float64(20), "portfolio": "https://github.com/ada", "phone": "+905321234567",
				"day": "2026-11-15", "share_sponsor": false}},
		{name: "only the required fields", answers: `{` + base + `}`,
			want: map[string]any{"motivation": "Go öğrenmek", "rules": true}},
		{name: "empty optional answers are no answers", answers: `{` + base + `,"nickname":"  ","topics":[],"phone":null}`,
			want: map[string]any{"motivation": "Go öğrenmek", "rules": true}},
		{name: "required missing", answers: `{}`,
			errs: map[string]string{"body.answers.motivation": "is required", "body.answers.rules": "is required"}},
		{name: "required blank text", answers: `{"motivation":"   ","rules":true}`,
			errs: map[string]string{"body.answers.motivation": "is required"}},
		{name: "nil answers", answers: `null`,
			errs: map[string]string{"body.answers.motivation": "is required", "body.answers.rules": "is required"}},
		{name: "max_length", answers: `{"motivation":"bu metin yirmi karakterden uzun","rules":true}`,
			errs: map[string]string{"body.answers.motivation": "at most 20 characters"}},
		{name: "option outside the list", answers: `{` + base + `,"tshirt":"xxl"}`,
			errs: map[string]string{"body.answers.tshirt": "not one of the options"}},
		{name: "deleted option", answers: `{` + base + `,"tshirt":"xl"}`,
			errs: map[string]string{"body.answers.tshirt": "not one of the options"}},
		{name: "unknown field", answers: `{` + base + `,"tckn":"12345678901"}`,
			errs: map[string]string{"body.answers.tckn": "is not a field of this form"}},
		{name: "unknown odd key never becomes a location", answers: `{` + base + `,"__proto__":1}`,
			errs: map[string]string{"body.answers": "is not a field of this form"}},
		{name: "number sent as a string", answers: `{` + base + `,"age":"20"}`,
			errs: map[string]string{"body.answers.age": "must be of type integer"}},
		{name: "integer only", answers: `{` + base + `,"age":20.5}`,
			errs: map[string]string{"body.answers.age": "must be of type integer"}},
		{name: "below the minimum", answers: `{` + base + `,"age":15}`,
			errs: map[string]string{"body.answers.age": "below the minimum"}},
		{name: "checkbox sent as a string", answers: `{"motivation":"x","rules":"true"}`,
			errs: map[string]string{"body.answers.rules": "must be of type boolean"}},
		{name: "required checkbox unchecked", answers: `{"motivation":"x","rules":false}`,
			errs: map[string]string{"body.answers.rules": "must be checked"}},
		{name: "pattern", answers: `{` + base + `,"nickname":"ADA"}`,
			errs: map[string]string{"body.answers.nickname": "required format"}},
		{name: "control character", answers: `{` + base + `,"nickname":"a\u0007b"}`,
			errs: map[string]string{"body.answers.nickname": "control characters"}},
		{name: "too many selections", answers: `{` + base + `,"topics":["go","sql","web"]}`,
			errs: map[string]string{"body.answers.topics": "at most 2 selections"}},
		{name: "repeated selection", answers: `{` + base + `,"topics":["go","go"]}`,
			errs: map[string]string{"body.answers.topics": "must not repeat"}},
		{name: "selection outside the list", answers: `{` + base + `,"topics":["rust"]}`,
			errs: map[string]string{"body.answers.topics": "not one of the options"}},
		{name: "multi_select sent as a string", answers: `{` + base + `,"topics":"go"}`,
			errs: map[string]string{"body.answers.topics": "must be of type array"}},
		{name: "http URL", answers: `{` + base + `,"portfolio":"http://github.com/ada"}`,
			errs: map[string]string{"body.answers.portfolio": "required format"}},
		{name: "URL on another host", answers: `{` + base + `,"portfolio":"https://evil.example/github.com"}`,
			errs: map[string]string{"body.answers.portfolio": "allowed sites"}},
		{name: "URL on a subdomain", answers: `{` + base + `,"portfolio":"https://gist.github.com/ada"}`,
			want: map[string]any{"motivation": "Go öğrenmek", "rules": true, "portfolio": "https://gist.github.com/ada"}},
		{name: "phone", answers: `{` + base + `,"phone":"+44 20 7946 0958"}`,
			want: map[string]any{"motivation": "Go öğrenmek", "rules": true, "phone": "+442079460958"}},
		{name: "not a phone", answers: `{` + base + `,"phone":"ara beni"}`,
			errs: map[string]string{"body.answers.phone": "phone number"}},
		{name: "date format", answers: `{` + base + `,"day":"15.11.2026"}`,
			errs: map[string]string{"body.answers.day": "valid date"}},
		{name: "date out of range", answers: `{` + base + `,"day":"2026-12-01"}`,
			errs: map[string]string{"body.answers.day": "must not be after 2026-11-30"}},
		{name: "consent sent as a string", answers: `{` + base + `,"share_sponsor":"evet"}`,
			errs: map[string]string{"body.answers.share_sponsor": "must be of type boolean"}},
		{name: "several errors at once", answers: `{"motivation":1,"rules":true,"age":"x","extra":true}`,
			errs: map[string]string{"body.answers.motivation": "must be of type string", "body.answers.extra": "not a field",
				"body.answers.age": "must be of type integer"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := events.ValidateAnswers(s, decodeAnswers(t, tc.answers))
			if len(tc.errs) == 0 {
				if err != nil {
					t.Fatalf("ValidateAnswers = %v %v", err, fieldsOf(err))
				}
				if tc.want != nil && !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("cleaned = %#v, want %#v", got, tc.want)
				}
				return
			}
			p, ok := errors.AsType[*problem.Error](err)
			if !ok || p.Status() != 422 || p.Code() != "validation_failed" {
				t.Fatalf("error = %v, want 422 validation_failed", err)
			}
			fs := p.Fields()
			if len(fs) != len(tc.errs) {
				t.Fatalf("fields = %v, want %v", fs, tc.errs)
			}
			for _, f := range fs {
				want, ok := tc.errs[f.Location]
				if !ok || !strings.Contains(f.Message, want) {
					t.Fatalf("field %s: %q, want %q (all %v)", f.Location, f.Message, want, fs)
				}
			}
		})
	}
}

// TestValidateAnswers_NeverEchoesTheValue: field errors carry fixed texts
// (04 §1.10), never the submitted answer.
func TestValidateAnswers_NeverEchoesTheValue(t *testing.T) {
	t.Parallel()
	s := mustSchema(t, answersForm)
	secret := "gizli-deger-42"
	_, err := events.ValidateAnswers(s, map[string]any{
		"motivation": strings.Repeat(secret, 3), "rules": true, "tshirt": secret, "nickname": secret, "age": secret,
		"portfolio": "https://" + secret + ".example", "phone": secret, "day": secret, secret: secret,
	})
	for _, f := range fieldsOf(err) {
		if strings.Contains(f.Message, secret) || strings.Contains(f.Location, secret) {
			t.Fatalf("field error echoes the answer: %+v", f)
		}
	}
	if len(fieldsOf(err)) < 8 {
		t.Fatalf("errors = %v", fieldsOf(err))
	}
}

// TestFormSchema_AnswersSchemaIsJSONSchema: the document handed to clients
// is JSON Schema 2020-12 with the form's keys and required list.
func TestFormSchema_AnswersSchemaIsJSONSchema(t *testing.T) {
	t.Parallel()
	doc := mustSchema(t, answersForm).AnswersSchema()
	if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" || doc["additionalProperties"] != false {
		t.Fatalf("doc = %v", doc)
	}
	props, _ := doc["properties"].(map[string]any)
	if len(props) != 10 {
		t.Fatalf("properties = %v", props)
	}
	req, _ := doc["required"].([]any)
	if !slices.Equal(req, []any{"motivation", "rules"}) {
		t.Fatalf("required = %v", req)
	}
	if _, err := json.Marshal(doc); err != nil {
		t.Fatal(err)
	}
}

// FuzzValidateAnswers: any answers against any valid schema never panic; a
// cleaned result passes again unchanged and only carries form keys.
func FuzzValidateAnswers(f *testing.F) {
	f.Add(answersForm, `{"motivation":"Go","rules":true,"age":20,"topics":["go"],"phone":"05321234567"}`)
	f.Add(answersForm, `{"motivation":"x","rules":true,"tshirt":"xl","day":"2026-11-31"}`)
	f.Add(answersForm, `{"motivation":"\u202e","rules":"true","age":1e400}`)
	f.Add(answersForm, `{"motivation":[1,2],"rules":{},"portfolio":"https://user@github.com"}`)
	f.Add(`{"schema_version":1,"fields":[{"key":"n","type":"number","label":{"tr":"N"},"required":true}]}`, `{"n":-0}`)
	f.Fuzz(func(t *testing.T, schemaJSON, answersJSON string) {
		s, err := events.ParseFormSchema([]byte(schemaJSON), "body.draft_schema")
		if err != nil {
			return
		}
		var answers map[string]any
		if json.Unmarshal([]byte(answersJSON), &answers) != nil {
			return
		}
		got, err := events.ValidateAnswers(s, answers)
		if err != nil {
			if p, ok := errors.AsType[*problem.Error](err); ok && p.Status() == 422 {
				return
			}
			t.Fatalf("ValidateAnswers: unexpected error %v", err)
		}
		for k := range got {
			if _, ok := s.Field(k); !ok {
				t.Fatalf("cleaned answers carry %q, not a field", k)
			}
		}
		again, err := events.ValidateAnswers(s, got)
		if err != nil || !reflect.DeepEqual(again, got) {
			t.Fatalf("cleaned answers do not pass again: %v %v\n%#v\n%#v", err, fieldsOf(err), got, again)
		}
	})
}
