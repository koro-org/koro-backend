package events_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"koro/internal/events"
	"koro/internal/platform/problem"
)

// TestParseFormSchema_Canonical: the parsed schema is canonical: texts are
// normalised, empty texts and rules dropped, and the same content in
// another key order or spacing hashes the same (schema_sha256).
func TestParseFormSchema_Canonical(t *testing.T) {
	t.Parallel()
	a := mustSchema(t, `{"fields":[{"required":true,"label":{"en":"Why?","tr":"  Neden\tkatılmak  istiyorsun? "},
		"type":"long_text","key":"motivation","validation":{}, "purpose":{"tr":""}}],"schema_version":1}`)
	b := mustSchema(t, `{"schema_version":1,"fields":[{"key":"motivation","type":"long_text",
		"label":{"tr":"Neden katılmak istiyorsun?","en":"Why?"},"required":true}]}`)
	if !bytes.Equal(a.Canonical(), b.Canonical()) || !bytes.Equal(a.SHA256(), b.SHA256()) {
		t.Fatalf("canonical forms differ:\n%s\n%s", a.Canonical(), b.Canonical())
	}
	f := a.Fields[0]
	if f.Label["tr"] != "Neden katılmak istiyorsun?" || f.Validation != nil || f.Purpose != nil {
		t.Fatalf("field = %+v", f)
	}
	again := mustSchema(t, string(a.Canonical()))
	if !bytes.Equal(again.Canonical(), a.Canonical()) {
		t.Fatal("parsing the canonical form changes it")
	}
}

// TestParseFormSchema_Limits: options, rules and texts outside their limits.
func TestParseFormSchema_Limits(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 201)
	for _, tc := range []struct{ name, field, loc string }{
		{"label too long", `{"key":"ad","type":"short_text","label":{"tr":"` + long + `"},"required":false}`, ".fields[0].label.tr"},
		{"label control character", `{"key":"ad","type":"short_text","label":{"tr":"a\u0001b"},"required":false}`, ".fields[0].label.tr"},
		{"max_length over the type limit", `{"key":"ad","type":"short_text","label":{"tr":"Ad"},"required":false,
			"validation":{"max_length":201}}`, ".fields[0].validation.max_length"},
		{"min over max", `{"key":"n","type":"number","label":{"tr":"N"},"required":false,"validation":{"min":5,"max":1}}`,
			".fields[0].validation.min"},
		{"fractional bound of an integer", `{"key":"n","type":"number","label":{"tr":"N"},"required":false,
			"validation":{"integer":true,"min":1.5}}`, ".fields[0].validation.min"},
		{"bound too large", `{"key":"n","type":"number","label":{"tr":"N"},"required":false,"validation":{"max":1e13}}`,
			".fields[0].validation.max"},
		{"bad date", `{"key":"d","type":"date","label":{"tr":"D"},"required":false,"validation":{"min_date":"2026-02-30"}}`,
			".fields[0].validation.min_date"},
		{"bad host", `{"key":"u","type":"url","label":{"tr":"U"},"required":false,"validation":{"allowed_hosts":["https://x.com"]}}`,
			".fields[0].validation.allowed_hosts[0]"},
		{"options on a text field", `{"key":"ad","type":"short_text","label":{"tr":"Ad"},"required":false,
			"options":[{"value":"a","label":{"tr":"A"}}]}`, ".fields[0].options"},
		{"bad option value", `{"key":"s","type":"single_select","label":{"tr":"S"},"required":false,
			"options":[{"value":"A B","label":{"tr":"A"}}]}`, ".fields[0].options[0].value"},
		{"repeated option value", `{"key":"s","type":"single_select","label":{"tr":"S"},"required":false,
			"options":[{"value":"a","label":{"tr":"A"}},{"value":"a","label":{"tr":"B"}}]}`, ".fields[0].options[1].value"},
		{"only deleted options", `{"key":"s","type":"single_select","label":{"tr":"S"},"required":false,
			"options":[{"value":"a","label":{"tr":"A"},"deleted":true}]}`, ".fields[0].options"},
		{"max_select over the options", `{"key":"s","type":"multi_select","label":{"tr":"S"},"required":false,
			"options":[{"value":"a","label":{"tr":"A"}}],"validation":{"max_select":2}}`, ".fields[0].validation.max_select"},
		{"consent purpose on a text field", `{"key":"ad","type":"short_text","label":{"tr":"Ad"},"required":false,
			"consent_purpose":"event_sponsor_share"}`, ".fields[0].consent_purpose"},
		{"en without tr", `{"key":"ad","type":"short_text","label":{"tr":"Ad"},"help":{"en":"Name"},"required":false}`,
			".fields[0].help.tr"},
	} {
		_, err := events.ParseFormSchema([]byte(`{"schema_version":1,"fields":[`+tc.field+`]}`), "s")
		p, ok := errors.AsType[*problem.Error](err)
		if !ok || p.Status() != 422 {
			t.Fatalf("%s: error = %v", tc.name, err)
		}
		found := false
		for _, f := range p.Fields() {
			found = found || f.Location == "s"+tc.loc
		}
		if !found {
			t.Fatalf("%s: fields = %v, want %s", tc.name, p.Fields(), "s"+tc.loc)
		}
	}
}

// FuzzValidateFormSchema: any input is either refused with 422 or parsed
// into a canonical schema that parses to itself, has at most 20 fields,
// derives a JSON Schema and lints without panicking (05 §13.6).
func FuzzValidateFormSchema(f *testing.F) {
	f.Add([]byte(answersForm))
	f.Add([]byte(`{"schema_version":1,"fields":[]}`))
	f.Add([]byte(`{"schema_version":1,"description":{"tr":"<p>Merhaba <script>x</script></p>"},"fields":[
		{"key":"bio","type":"long_text","label":{"tr":"Sen"},"help":{"tr":"<a href=\"https://koro.test\">x</a> &amp; <b>"},"required":false}]}`))
	f.Add([]byte(`{"schema_version":1,"fields":[{"key":"__proto__","type":"short_text","label":{"tr":"x"},"required":false}]}`))
	f.Add([]byte(`{"schema_version":1,"fields":[{"key":"p","type":"short_text","label":{"tr":"x"},"required":false,
		"validation":{"pattern":"(a+)+$"}}]}`))
	f.Add([]byte(`{"schema_version":1,"fields":null}`))
	f.Add([]byte(`{"schema_version":1,"fields":[{"key":"n","type":"number","label":{"tr":"N"},"required":true,
		"validation":{"min":-1e12,"max":1e12,"integer":true}}]} trailing`))
	f.Add([]byte(`{"sChemA_version":1,"desCription":{"tr":"0 <A> 0"},"fields":[]}`))
	rules := events.DefaultLintRules()
	f.Fuzz(func(t *testing.T, raw []byte) {
		s, err := events.ParseFormSchema(raw, "body.draft_schema")
		if err != nil {
			if p, ok := errors.AsType[*problem.Error](err); !ok || p.Status() != 422 {
				t.Fatalf("ParseFormSchema: unexpected error %v", err)
			}
			return
		}
		if len(s.Fields) > events.MaxFormFields {
			t.Fatalf("%d fields", len(s.Fields))
		}
		canon := s.Canonical()
		again, err := events.ParseFormSchema(canon, "body.draft_schema")
		if err != nil {
			t.Fatalf("the canonical form is refused: %v %v\n%s", err, fieldsOf(err), canon)
		}
		if !bytes.Equal(again.Canonical(), canon) {
			t.Fatalf("canonical form is not stable:\n%s\n%s", canon, again.Canonical())
		}
		if _, err := events.DecodeFormSchema(canon); err != nil {
			t.Fatal(err)
		}
		_ = s.AnswersSchema()
		_ = events.LintSchema(s, rules)
		if _, err := events.ValidateAnswers(s, map[string]any{}); err != nil {
			if p, ok := errors.AsType[*problem.Error](err); !ok || p.Status() != 422 {
				t.Fatalf("ValidateAnswers(empty): %v", err)
			}
		}
	})
}
