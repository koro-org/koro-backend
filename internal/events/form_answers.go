package events

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/unicode/norm"

	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Answer validation (ADR-0013, R-003 §2.5.1, 05 §14 "sunucu tarafı
// doğrulama her zaman"): the single server-side check of a registration's
// answers, used by BE-028 against the version the registration is made on.
// Two layers:
//
//  1. AnswersSchema derives a JSON Schema 2020-12 document from the form
//     (types, required, lengths, the RE2 pattern, options, selection
//     counts, numeric bounds, date format, additionalProperties false);
//     it is compiled with santhosh-tekuri/jsonschema (Go regexp, so RE2,
//     no ReDoS) and cached by the schema hash. Clients receive the same
//     document (getEventForm answers_schema).
//  2. The rules JSON Schema cannot express: text normalisation (05 §9.5),
//     https-only URLs and their allowed hosts, E.164 phone numbers, the
//     date range.
//
// Field errors are "body.answers.<key>" with a fixed message; the submitted
// value is never echoed (04 §1.10).

// answersLoc is the location prefix of answer errors (createRegistration
// body, 04 §E.2).
const answersLoc = "body.answers."

// maxAnswerNumber bounds every number answer (exact in a float64).
const maxAnswerNumber = 1e15

// ValidateAnswers checks answers (a JSON object decoded with encoding/json:
// strings, float64 or json.Number, booleans, arrays) against a published
// form schema and returns the cleaned answers: texts normalised, phone
// numbers in E.164, numbers as float64, multi_select as an array of
// strings; empty texts and nulls are left out. Errors are 422
// validation_failed with one error per field. The schema is not modified.
func ValidateAnswers(s FormSchema, answers map[string]any) (map[string]any, error) {
	errs := answerErrors{order: map[string]int{}}
	for i, f := range s.Fields {
		errs.order[f.Key] = i
	}
	inst := map[string]any{}
	for _, f := range s.Fields {
		v, ok := answers[f.Key]
		if !ok || v == nil {
			continue
		}
		v, present, msg := prepareAnswer(f, v)
		switch {
		case msg != "":
			errs.add(f.Key, msg)
		case present:
			inst[f.Key] = v
		}
	}
	for k := range answers {
		if _, ok := errs.order[k]; !ok {
			errs.add(k, "is not a field of this form")
		}
	}
	// The schema layer sees the known answers only (an unknown key is
	// reported above), so every field gets its own message.
	sch, err := compiledAnswers(s)
	if err != nil {
		return nil, err
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if !errors.As(err, &ve) {
			return nil, fmt.Errorf("events: validate answers: %w", err)
		}
		errs.fromSchema(ve)
	}
	out := map[string]any{}
	for _, f := range s.Fields {
		v, ok := inst[f.Key]
		if !ok || errs.has(f.Key) {
			continue
		}
		clean, msg := finishAnswer(f, v)
		if msg != "" {
			errs.add(f.Key, msg)
			continue
		}
		out[f.Key] = clean
	}
	if !errs.empty() {
		return nil, problem.ErrValidation.WithFields(errs.fields()...)
	}
	return out, nil
}

// prepareAnswer normalises text answers before the schema check; an empty
// text or an empty selection is no answer (present false).
func prepareAnswer(f FormField, v any) (any, bool, string) {
	switch f.Type {
	case FieldShortText:
		s, ok := v.(string)
		if !ok {
			return v, true, ""
		}
		n, err := textx.NormalizeText(s)
		if err != nil {
			return nil, false, "contains control characters"
		}
		return n, n != "", ""
	case FieldLongText:
		s, ok := v.(string)
		if !ok {
			return v, true, ""
		}
		n, ok := normalizeMultiline(s)
		if !ok {
			return nil, false, "contains control characters"
		}
		return n, n != "", ""
	case FieldURL, FieldPhone, FieldDate:
		s, ok := v.(string)
		if !ok {
			return v, true, ""
		}
		s = strings.TrimSpace(s)
		return s, s != "", ""
	case FieldMultiSelect:
		if a, ok := v.([]any); ok && len(a) == 0 {
			return nil, false, ""
		}
	case FieldNumber:
		if n, ok := v.(json.Number); ok {
			f, err := n.Float64()
			if err != nil {
				return nil, false, "must be a number"
			}
			return f, true, ""
		}
	}
	return v, true, ""
}

// finishAnswer applies the rules JSON Schema does not express and returns
// the stored form of an answer that passed the schema.
func finishAnswer(f FormField, v any) (any, string) {
	val := FormFieldValidation{}
	if f.Validation != nil {
		val = *f.Validation
	}
	switch f.Type {
	case FieldNumber:
		x, ok := v.(float64)
		if !ok || math.IsInf(x, 0) || math.IsNaN(x) {
			return nil, "must be a number"
		}
		return x, ""
	case FieldURL:
		s, _ := v.(string)
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !strings.HasPrefix(s, "https://") {
			return nil, "must be an https:// address"
		}
		if len(val.AllowedHosts) > 0 && !hostAllowed(strings.ToLower(u.Hostname()), val.AllowedHosts) {
			return nil, "must be an address on one of the allowed sites"
		}
		return s, ""
	case FieldPhone:
		s, _ := v.(string)
		p, ok := normalizePhone(s)
		if !ok {
			return nil, "must be a phone number"
		}
		return p, ""
	case FieldDate:
		s, _ := v.(string)
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			return nil, "must be a YYYY-MM-DD date"
		}
		if lo, err := time.Parse(time.DateOnly, val.MinDate); err == nil && d.Before(lo) {
			return nil, "must not be before " + val.MinDate
		}
		if hi, err := time.Parse(time.DateOnly, val.MaxDate); err == nil && d.After(hi) {
			return nil, "must not be after " + val.MaxDate
		}
		return s, ""
	case FieldMultiSelect:
		a, _ := v.([]any)
		out := make([]any, 0, len(a))
		for _, x := range a {
			s, ok := x.(string)
			if !ok {
				return nil, "must be a list of options"
			}
			out = append(out, s)
		}
		return out, ""
	}
	return v, ""
}

func hostAllowed(host string, allowed []string) bool {
	for _, h := range allowed {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// normalizeMultiline is NormalizeText for a long text: line breaks (\n)
// and tabs stay, CRLF becomes LF, every other control or bidirectional
// override character is refused, zero-width characters are dropped, the
// text is trimmed and NFC.
func normalizeMultiline(s string) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
		case r == '\u00ad' || r == '\u200b' || r == '\u2060' || r == '\ufeff':
			continue
		case unicode.IsControl(r), r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
			return "", false
		}
		b.WriteRune(r)
	}
	return norm.NFC.String(strings.TrimSpace(b.String())), true
}

// normalizePhone returns the E.164 form of a phone number (R-003 §2.5.1,
// Turkey the default region): spaces, dots, hyphens and parentheses are
// dropped; "00" is the international prefix; a national number is
// 0 + 10 digits or 10 digits.
func normalizePhone(s string) (string, bool) {
	if len(s) > maxPhoneInput {
		return "", false
	}
	var b strings.Builder
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && i == 0:
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')':
		default:
			return "", false
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(d, "+"):
		d = d[1:]
	case strings.HasPrefix(d, "00"):
		d = d[2:]
	case len(d) == 11 && d[0] == '0':
		d = "90" + d[1:]
	case len(d) == 10 && d[0] != '0':
		d = "90" + d
	default:
		return "", false
	}
	if len(d) < 8 || len(d) > 15 || d[0] == '0' || strings.ContainsRune(d, '+') {
		return "", false
	}
	return "+" + d, true
}

// AnswersSchema is the JSON Schema 2020-12 document of a form's answers
// object (the first validation layer; getEventForm answers_schema).
func (s FormSchema) AnswersSchema() map[string]any {
	props := map[string]any{}
	required := []any{}
	for _, f := range s.Fields {
		val := FormFieldValidation{}
		if f.Validation != nil {
			val = *f.Validation
		}
		p := map[string]any{}
		switch f.Type {
		case FieldShortText, FieldLongText:
			limit := maxShortText
			if f.Type == FieldLongText {
				limit = maxLongText
			}
			if val.MaxLength != nil {
				limit = *val.MaxLength
			}
			p["type"], p["maxLength"] = "string", limit
			if val.MinLength != nil {
				p["minLength"] = *val.MinLength
			}
			if f.Type == FieldShortText && val.Pattern != "" {
				p["pattern"] = val.Pattern
			}
		case FieldSingleSelect:
			p["type"], p["enum"] = "string", anySlice(liveOptions(f.Options))
		case FieldMultiSelect:
			live := liveOptions(f.Options)
			p["type"], p["uniqueItems"] = "array", true
			p["items"] = map[string]any{"type": "string", "enum": anySlice(live)}
			lo, hi := 0, len(live)
			if f.Required {
				lo = 1
			}
			if val.MinSelect != nil && *val.MinSelect > lo {
				lo = *val.MinSelect
			}
			if val.MaxSelect != nil {
				hi = *val.MaxSelect
			}
			p["minItems"], p["maxItems"] = lo, hi
		case FieldCheckbox, FieldConsent:
			p["type"] = "boolean"
			if f.Required {
				p["const"] = true
			}
		case FieldNumber:
			p["type"] = "number"
			if val.Integer {
				p["type"] = "integer"
			}
			lo, hi := -maxAnswerNumber, maxAnswerNumber
			if val.Min != nil {
				lo = *val.Min
			}
			if val.Max != nil {
				hi = *val.Max
			}
			p["minimum"], p["maximum"] = lo, hi
		case FieldURL:
			p["type"], p["maxLength"], p["pattern"] = "string", maxURL, "^https://"
		case FieldPhone:
			p["type"], p["maxLength"] = "string", maxPhoneInput
		case FieldDate:
			p["type"], p["format"] = "string", "date"
		}
		props[f.Key] = p
		if f.Required {
			required = append(required, f.Key)
		}
	}
	return map[string]any{
		"$schema":              "https://json-schema.org/draft/2020-12/schema",
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             required,
	}
}

func anySlice(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// answerSchemas caches the compiled answer schemas by schema hash; a full
// cache is emptied (versions are few, the cache only saves compile time).
var answerSchemas = struct {
	sync.Mutex
	m map[string]*jsonschema.Schema
}{m: map[string]*jsonschema.Schema{}}

const maxCachedSchemas = 512

// answerSchemaURL names the in-memory document (never fetched).
const answerSchemaURL = "urn:koro:event-form-answers"

func compiledAnswers(s FormSchema) (*jsonschema.Schema, error) {
	key := hex.EncodeToString(s.SHA256())
	answerSchemas.Lock()
	sch, ok := answerSchemas.m[key]
	answerSchemas.Unlock()
	if ok {
		return sch, nil
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(answerSchemaURL, s.AnswersSchema()); err != nil {
		return nil, fmt.Errorf("events: answer schema: %w", err)
	}
	sch, err := c.Compile(answerSchemaURL)
	if err != nil {
		return nil, fmt.Errorf("events: answer schema: %w", err)
	}
	answerSchemas.Lock()
	if len(answerSchemas.m) >= maxCachedSchemas {
		clear(answerSchemas.m)
	}
	answerSchemas.m[key] = sch
	answerSchemas.Unlock()
	return sch, nil
}

// answerErrors keeps one message per answer key, in form order (unknown
// keys last, sorted).
type answerErrors struct {
	order map[string]int
	msgs  map[string]string
}

func (e *answerErrors) add(key, msg string) {
	if e.msgs == nil {
		e.msgs = map[string]string{}
	}
	if _, dup := e.msgs[key]; !dup {
		e.msgs[key] = msg
	}
}

func (e *answerErrors) empty() bool { return len(e.msgs) == 0 }

func (e *answerErrors) has(key string) bool {
	_, ok := e.msgs[key]
	return ok
}

func (e *answerErrors) fields() []problem.FieldError {
	keys := make([]string, 0, len(e.msgs))
	for k := range e.msgs {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		if i, ok := e.order[k]; ok {
			return i
		}
		return len(e.order)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := rank(keys[i]), rank(keys[j])
		if ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	out := make([]problem.FieldError, 0, min(len(keys), maxFormErrors))
	for _, k := range keys {
		if len(out) == maxFormErrors {
			break
		}
		loc := answersLoc + k
		if _, known := e.order[k]; !known && !fieldKeyRe.MatchString(k) {
			loc = "body.answers" // an arbitrary key never becomes a location
		}
		out = append(out, problem.FieldError{Location: loc, Message: e.msgs[k]})
	}
	return out
}

// fromSchema maps the leaf errors of the JSON Schema check onto answer
// keys with fixed messages (the library's texts may quote the value).
func (e *answerErrors) fromSchema(ve *jsonschema.ValidationError) {
	var leaves []*jsonschema.ValidationError
	var walk func(*jsonschema.ValidationError)
	walk = func(v *jsonschema.ValidationError) {
		if len(v.Causes) == 0 {
			leaves = append(leaves, v)
			return
		}
		for _, c := range v.Causes {
			walk(c)
		}
	}
	walk(ve)
	for _, l := range leaves {
		switch k := l.ErrorKind.(type) {
		case *kind.Required:
			for _, m := range k.Missing {
				e.add(m, "is required")
			}
			continue
		case *kind.AdditionalProperties:
			for _, p := range k.Properties {
				e.add(p, "is not a field of this form")
			}
			continue
		}
		if len(l.InstanceLocation) == 0 {
			e.add("", "is invalid")
			continue
		}
		e.add(l.InstanceLocation[0], schemaMessage(l.ErrorKind))
	}
}

// schemaMessage is the fixed message of a JSON Schema error kind; limits
// come from the form, never from the answer.
func schemaMessage(k jsonschema.ErrorKind) string {
	switch k := k.(type) {
	case *kind.Type:
		want := slices.Clone(k.Want)
		slices.Sort(want)
		return "must be of type " + strings.Join(want, " or ")
	case *kind.MaxLength:
		return "must be at most " + strconv.Itoa(k.Want) + " characters"
	case *kind.MinLength:
		return "must be at least " + strconv.Itoa(k.Want) + " characters"
	case *kind.Pattern:
		return "does not match the required format"
	case *kind.Enum:
		return "is not one of the options"
	case *kind.Const:
		return "must be checked"
	case *kind.MinItems:
		return "must have at least " + strconv.Itoa(k.Want) + " selections"
	case *kind.MaxItems:
		return "must have at most " + strconv.Itoa(k.Want) + " selections"
	case *kind.UniqueItems:
		return "must not repeat an option"
	case *kind.Minimum, *kind.ExclusiveMinimum:
		return "is below the minimum"
	case *kind.Maximum, *kind.ExclusiveMaximum:
		return "is above the maximum"
	case *kind.Format:
		return "is not a valid " + k.Want
	}
	return "is invalid"
}
