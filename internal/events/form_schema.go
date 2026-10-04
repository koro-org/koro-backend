package events

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"koro/internal/consent"
	"koro/internal/platform/problem"
	"koro/internal/platform/sanitize"
	"koro/internal/platform/textx"
)

// Form schema (R-003 §2.5.1, ADR-0013, 01 §4.10). A form is a JSON document
// of at most 20 typed fields; every published version is frozen in
// event_form_versions.schema, and the server derives a JSON Schema 2020-12
// document from it to validate the answers (form_answers.go). Answers keep
// their JSON types: a number is a number, a checkbox a boolean, a
// multi_select an array (the "every field is a string" mistake of the
// Sky Lab forms is not repeated, R-001 §2.3).
//
// The parser is strict: unknown properties, unknown field types, options or
// validation keys a type does not take, keys outside ^[a-z][a-z0-9_]{1,40}$
// and more than 20 fields are refused (422 with one error per problem).
// Labels, purposes and option labels are plain single-line text
// (textx.NormalizeText); help texts and the form description are HTML
// fragments cleaned with the platform allow-list (platform/sanitize). The
// parsed schema is canonical: its JSON encoding is what a version stores
// and what schema_sha256 hashes.

// Limits of a form schema.
const (
	// FormSchemaVersion is the only schema_version of v1.
	FormSchemaVersion = 1
	// MaxFormFields is the field limit of a form (02 §11.4 field_count).
	MaxFormFields = 20

	maxFormSchemaBytes = 64 << 10
	maxFieldLabel      = 200
	maxFieldHelp       = 1000
	maxFieldPurpose    = 300
	maxFormDescription = 4000
	maxOptions         = 50
	maxOptionLabel     = 100
	maxShortText       = 200
	maxLongText        = 2000
	maxTextPattern     = 200
	maxAllowedHosts    = 10
	maxNumberAbs       = 1e12
	maxFormErrors      = 50
	maxPhoneInput      = 32
)

// FieldType is the type of a form field (R-003 §2.5.1).
type FieldType string

// Field types and the JSON type of their answer.
const (
	FieldShortText    FieldType = "short_text"    // string, one line, ≤ 200
	FieldLongText     FieldType = "long_text"     // string, multi-line, ≤ 2000
	FieldSingleSelect FieldType = "single_select" // string: an option value
	FieldMultiSelect  FieldType = "multi_select"  // array of option values
	FieldCheckbox     FieldType = "checkbox"      // boolean (required: must be true)
	FieldNumber       FieldType = "number"        // number (integer flag, min/max)
	FieldURL          FieldType = "url"           // string: https:// only
	FieldPhone        FieldType = "phone"         // string, normalised to E.164
	FieldDate         FieldType = "date"          // string: YYYY-MM-DD
	FieldConsent      FieldType = "consent"       // boolean: a per-registration consent
)

// FieldTypes lists every field type.
func FieldTypes() []FieldType {
	return []FieldType{FieldShortText, FieldLongText, FieldSingleSelect, FieldMultiSelect, FieldCheckbox, FieldNumber,
		FieldURL, FieldPhone, FieldDate, FieldConsent}
}

// Valid reports whether t is a field type of v1.
func (t FieldType) Valid() bool { return slices.Contains(FieldTypes(), t) }

func (t FieldType) selectable() bool { return t == FieldSingleSelect || t == FieldMultiSelect }

// LocalText is a text per locale: "tr" (required whenever the text is
// given) and optionally "en".
type LocalText map[string]string

// formLocales are the locales of a LocalText (tr first).
var formLocales = []string{"tr", "en"}

// FormFieldOption is an option of a select field. Value is the stable answer
// (a slug); a deleted option keeps its label for earlier answers but can
// no longer be chosen (R-003 §2.5.2).
type FormFieldOption struct {
	Value   string    `json:"value" pattern:"^[a-z0-9][a-z0-9_-]{0,39}$" doc:"Stable answer value"`
	Label   LocalText `json:"label" doc:"Option text per locale (tr required)"`
	Deleted bool      `json:"deleted,omitempty" doc:"Kept for earlier answers; cannot be chosen"`
}

// FormFieldValidation holds the type-specific rules of a field; a key a type
// does not take is refused.
type FormFieldValidation struct {
	MinLength    *int     `json:"min_length,omitempty" minimum:"0" doc:"short_text, long_text"`
	MaxLength    *int     `json:"max_length,omitempty" minimum:"1" doc:"short_text (≤ 200), long_text (≤ 2000)"`
	Pattern      string   `json:"pattern,omitempty" maxLength:"200" doc:"short_text: RE2 expression the answer must match"`
	Integer      bool     `json:"integer,omitempty" doc:"number: whole numbers only"`
	Min          *float64 `json:"min,omitempty" doc:"number: lower bound (inclusive)"`
	Max          *float64 `json:"max,omitempty" doc:"number: upper bound (inclusive)"`
	MinSelect    *int     `json:"min_select,omitempty" minimum:"0" doc:"multi_select"`
	MaxSelect    *int     `json:"max_select,omitempty" minimum:"1" doc:"multi_select"`
	MinDate      string   `json:"min_date,omitempty" doc:"date: earliest YYYY-MM-DD (inclusive)"`
	MaxDate      string   `json:"max_date,omitempty" doc:"date: latest YYYY-MM-DD (inclusive)"`
	AllowedHosts []string `json:"allowed_hosts,omitempty" maxItems:"10" doc:"url: allowed host names (subdomains included)"`
}

// used returns the JSON names of the rules that are set.
func (v FormFieldValidation) used() []string {
	var out []string
	for _, r := range []struct {
		name string
		set  bool
	}{
		{"min_length", v.MinLength != nil}, {"max_length", v.MaxLength != nil}, {"pattern", v.Pattern != ""},
		{"integer", v.Integer}, {"min", v.Min != nil}, {"max", v.Max != nil}, {"min_select", v.MinSelect != nil},
		{"max_select", v.MaxSelect != nil}, {"min_date", v.MinDate != ""}, {"max_date", v.MaxDate != ""},
		{"allowed_hosts", len(v.AllowedHosts) > 0},
	} {
		if r.set {
			out = append(out, r.name)
		}
	}
	return out
}

// validationKeys are the rules each type takes.
var validationKeys = map[FieldType][]string{
	FieldShortText:   {"min_length", "max_length", "pattern"},
	FieldLongText:    {"min_length", "max_length"},
	FieldMultiSelect: {"min_select", "max_select"},
	FieldNumber:      {"integer", "min", "max"},
	FieldDate:        {"min_date", "max_date"},
	FieldURL:         {"allowed_hosts"},
}

// FormField is one field of a form.
type FormField struct {
	Key            string               `json:"key" pattern:"^[a-z][a-z0-9_]{1,40}$" doc:"Answer key; never reused with another type"`
	Type           FieldType            `json:"type" enum:"short_text,long_text,single_select,multi_select,checkbox,number,url,phone,date,consent"`
	Label          LocalText            `json:"label,omitempty" doc:"Plain text per locale (tr required; optional for consent fields)"`
	Help           LocalText            `json:"help,omitempty" doc:"HTML fragment per locale, cleaned by the server's allow-list"`
	Purpose        LocalText            `json:"purpose,omitempty" doc:"Why the field is asked (KVKK), shown to the user"`
	Required       bool                 `json:"required"`
	Validation     *FormFieldValidation `json:"validation,omitempty"`
	Options        []FormFieldOption    `json:"options,omitempty" maxItems:"50" doc:"single_select, multi_select"`
	ConsentPurpose string               `json:"consent_purpose,omitempty" doc:"consent: the consent purpose (event_sponsor_share)"`
}

// FormSchema is a registration form (R-003 §2.5.1).
type FormSchema struct {
	SchemaVersion int         `json:"schema_version" enum:"1"`
	Description   LocalText   `json:"description,omitempty" doc:"HTML fragment shown above the form, cleaned by the server's allow-list"`
	Fields        []FormField `json:"fields" maxItems:"20"`
}

// formConsentPurposes are the consent purposes a form may ask: the
// per-registration purposes of consent_purposes (scope_type registration,
// 02 §4.1). minorRestricted: a minor never sees the field (03 k12, T-22,
// 04 §E.1). A new per-registration purpose joins this list with its seed.
var formConsentPurposes = map[consent.Purpose]struct{ minorRestricted bool }{
	consent.PurposeEventSponsor: {minorRestricted: true},
}

var (
	fieldKeyRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{1,40}$`)
	optionValueRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
	hostRe        = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	// reservedKeys collide with JavaScript object internals in clients
	// (R-003 §2.5.1; __proto__ already fails the pattern).
	reservedKeys = []string{"constructor", "prototype"}
)

// ParseFormSchema decodes and checks a draft schema (putEventFormDraft and
// publication): strict JSON, the limits above, cleaned texts. loc prefixes
// the field error locations ("body.draft_schema"). The result is canonical
// (FormSchema.Canonical). Errors are 422 validation_failed.
func ParseFormSchema(raw []byte, loc string) (FormSchema, error) {
	if len(raw) > maxFormSchemaBytes {
		return FormSchema{}, fieldErr(loc, "must be at most 64 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s FormSchema
	if err := dec.Decode(&s); err != nil {
		return FormSchema{}, fieldErr(loc, "is not a form schema: "+jsonReason(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return FormSchema{}, fieldErr(loc, "must be a single JSON object")
	}
	c := &schemaChecker{}
	out := c.schema(loc, s)
	if len(c.errs) > 0 {
		return FormSchema{}, problem.ErrValidation.WithFields(c.errs...)
	}
	return out, nil
}

// DecodeFormSchema decodes a stored schema (a draft or a published version)
// without re-running the draft rules: a version stays readable when the
// rules change later (R-003 §2.5.2).
func DecodeFormSchema(raw []byte) (FormSchema, error) {
	var s FormSchema
	if err := json.Unmarshal(raw, &s); err != nil {
		return FormSchema{}, errors.New("events: stored form schema is not a form schema")
	}
	if s.Fields == nil {
		s.Fields = []FormField{}
	}
	return s, nil
}

// jsonReason describes a decoding error without the submitted value (04
// §1.10: field errors never carry it).
func jsonReason(err error) string {
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return typeErr.Field + " has the wrong type"
		}
		return "a property has the wrong type"
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return "has an unknown property"
	}
	return "malformed JSON"
}

// Canonical is the JSON encoding of a parsed schema: the bytes a version
// stores and schema_sha256 hashes (struct order, sorted locale keys).
func (s FormSchema) Canonical() []byte {
	b, err := json.Marshal(s)
	if err != nil { // a parsed schema has finite numbers only
		panic("events: form schema does not encode: " + err.Error())
	}
	return b
}

// SHA256 is the hash of the canonical encoding (schema_sha256).
func (s FormSchema) SHA256() []byte {
	h := sha256.Sum256(s.Canonical())
	return h[:]
}

// Field returns the field of key.
func (s FormSchema) Field(key string) (FormField, bool) {
	for _, f := range s.Fields {
		if f.Key == key {
			return f, true
		}
	}
	return FormField{}, false
}

// ForReader is the schema a reader is shown and answers against: a minor
// never sees (and so cannot grant) a minor-restricted consent field (03
// k12, T-22, 04 §E.1). BE-028 validates a minor's answers against it.
func (s FormSchema) ForReader(minor bool) FormSchema {
	out := s
	out.Fields = make([]FormField, 0, len(s.Fields))
	for _, f := range s.Fields {
		if minor && f.Type == FieldConsent && formConsentPurposes[consent.Purpose(f.ConsentPurpose)].minorRestricted {
			continue
		}
		out.Fields = append(out.Fields, f)
	}
	return out
}

// ConsentPurposes returns the consent purposes of the consent fields.
func (s FormSchema) ConsentPurposes() []consent.Purpose {
	var out []consent.Purpose
	for _, f := range s.Fields {
		if f.Type == FieldConsent {
			out = append(out, consent.Purpose(f.ConsentPurpose))
		}
	}
	return out
}

// schemaChecker collects the field errors of a draft schema.
type schemaChecker struct{ errs []problem.FieldError }

func (c *schemaChecker) bad(loc, msg string) {
	if len(c.errs) < maxFormErrors {
		c.errs = append(c.errs, problem.FieldError{Location: loc, Message: msg})
	}
}

func (c *schemaChecker) schema(loc string, s FormSchema) FormSchema {
	out := FormSchema{SchemaVersion: FormSchemaVersion, Fields: []FormField{}}
	if s.SchemaVersion != FormSchemaVersion {
		c.bad(loc+".schema_version", "must be 1")
	}
	out.Description = c.text(loc+".description", s.Description, false, maxFormDescription, true)
	switch {
	case s.Fields == nil:
		c.bad(loc+".fields", "must be an array")
		return out
	case len(s.Fields) > MaxFormFields:
		c.bad(loc+".fields", "must have at most "+strconv.Itoa(MaxFormFields)+" fields")
		return out
	}
	keys, purposes := map[string]bool{}, map[string]bool{}
	for i, f := range s.Fields {
		fl := loc + ".fields[" + strconv.Itoa(i) + "]"
		cf := c.field(fl, f)
		if keys[f.Key] {
			c.bad(fl+".key", "is used by another field")
		}
		keys[f.Key] = true
		if f.Type == FieldConsent && f.ConsentPurpose != "" {
			if purposes[f.ConsentPurpose] {
				c.bad(fl+".consent_purpose", "is asked by another field")
			}
			purposes[f.ConsentPurpose] = true
		}
		out.Fields = append(out.Fields, cf)
	}
	return out
}

func (c *schemaChecker) field(loc string, f FormField) FormField {
	out := FormField{Key: f.Key, Type: f.Type, Required: f.Required}
	if !fieldKeyRe.MatchString(f.Key) || slices.Contains(reservedKeys, f.Key) {
		c.bad(loc+".key", "must match ^[a-z][a-z0-9_]{1,40}$ and not be a reserved name")
	}
	if !f.Type.Valid() {
		c.bad(loc+".type", "is not a field type")
		return out
	}
	out.Label = c.text(loc+".label", f.Label, f.Type != FieldConsent, maxFieldLabel, false)
	out.Help = c.text(loc+".help", f.Help, false, maxFieldHelp, true)
	out.Purpose = c.text(loc+".purpose", f.Purpose, false, maxFieldPurpose, false)

	if f.Type.selectable() {
		out.Options = c.options(loc+".options", f.Options)
	} else if len(f.Options) > 0 {
		c.bad(loc+".options", "are only taken by single_select and multi_select")
	}
	if f.Type == FieldConsent {
		if _, ok := formConsentPurposes[consent.Purpose(f.ConsentPurpose)]; !ok {
			c.bad(loc+".consent_purpose", "must be a per-registration consent purpose (event_sponsor_share)")
		}
		// KVKK: explicit consent is never a condition of the registration
		// (R-003 §2.6-3: separate, optional, not pre-ticked).
		if f.Required {
			c.bad(loc+".required", "a consent field must be optional")
		}
		out.ConsentPurpose = f.ConsentPurpose
	} else if f.ConsentPurpose != "" {
		c.bad(loc+".consent_purpose", "is only taken by consent fields")
	}
	if f.Validation != nil {
		out.Validation = c.validation(loc+".validation", f, *f.Validation, out.Options)
	}
	return out
}

// text cleans a LocalText: plain single-line text, or an HTML fragment
// through the platform allow-list. An empty text is dropped.
func (c *schemaChecker) text(loc string, t LocalText, required bool, maxRunes int, html bool) LocalText {
	if len(t) == 0 {
		if required {
			c.bad(loc, "is required (tr)")
		}
		return nil
	}
	out := LocalText{}
	for k := range t {
		if !slices.Contains(formLocales, k) {
			c.bad(loc, "takes the locales tr and en only")
			return nil
		}
	}
	for _, k := range formLocales {
		v, ok := t[k]
		if !ok {
			continue
		}
		clean, ok := cleanLocal(v, maxRunes, html)
		switch {
		case !ok:
			c.bad(loc+"."+k, "must be at most "+strconv.Itoa(maxRunes)+" characters without control characters")
		case clean != "":
			out[k] = clean
		}
	}
	if len(out) == 0 {
		if required {
			c.bad(loc, "is required (tr)")
		}
		return nil
	}
	if _, ok := out["tr"]; !ok {
		c.bad(loc+".tr", "is required when the text is given")
	}
	return out
}

// cleanLocal normalises one text (05 §9.5); html runs the allow-list
// sanitiser (platform/sanitize) on it, until the result is stable (a
// removed tag can leave spaces to collapse), so that the canonical schema
// parses to itself. "" is a valid (empty) result.
func cleanLocal(s string, maxRunes int, html bool) (string, bool) {
	if len(s) > maxRunes*8 {
		return "", false
	}
	n, err := textx.NormalizeText(s)
	if err != nil {
		return "", false
	}
	for i := 0; html && i < 4; i++ {
		h, err := textx.NormalizeText(sanitize.HTML(n))
		if err != nil {
			return "", false
		}
		if h == n {
			break
		}
		n = h
	}
	if utf8.RuneCountInString(n) > maxRunes {
		return "", false
	}
	return n, true
}

func (c *schemaChecker) options(loc string, opts []FormFieldOption) []FormFieldOption {
	switch {
	case len(opts) == 0:
		c.bad(loc, "must have at least one option")
		return nil
	case len(opts) > maxOptions:
		c.bad(loc, "must have at most "+strconv.Itoa(maxOptions)+" options")
		return nil
	}
	out := make([]FormFieldOption, 0, len(opts))
	seen, live := map[string]bool{}, 0
	for i, o := range opts {
		ol := loc + "[" + strconv.Itoa(i) + "]"
		if !optionValueRe.MatchString(o.Value) {
			c.bad(ol+".value", "must match ^[a-z0-9][a-z0-9_-]{0,39}$")
		}
		if seen[o.Value] {
			c.bad(ol+".value", "is used by another option")
		}
		seen[o.Value] = true
		if !o.Deleted {
			live++
		}
		out = append(out, FormFieldOption{Value: o.Value, Label: c.text(ol+".label", o.Label, true, maxOptionLabel, false), Deleted: o.Deleted})
	}
	if live == 0 {
		c.bad(loc, "must have an option that is not deleted")
	}
	return out
}

func liveOptions(opts []FormFieldOption) []string {
	out := []string{}
	for _, o := range opts {
		if !o.Deleted {
			out = append(out, o.Value)
		}
	}
	return out
}

func (c *schemaChecker) validation(loc string, f FormField, v FormFieldValidation, opts []FormFieldOption) *FormFieldValidation {
	allowed := validationKeys[f.Type]
	for _, k := range v.used() {
		if !slices.Contains(allowed, k) {
			c.bad(loc+"."+k, "is not a rule of "+string(f.Type))
		}
	}
	out := FormFieldValidation{}
	switch f.Type {
	case FieldShortText, FieldLongText:
		limit := maxShortText
		if f.Type == FieldLongText {
			limit = maxLongText
		}
		out.MinLength = c.intIn(loc+".min_length", v.MinLength, 0, limit)
		out.MaxLength = c.intIn(loc+".max_length", v.MaxLength, 1, limit)
		if out.MinLength != nil && out.MaxLength != nil && *out.MinLength > *out.MaxLength {
			c.bad(loc+".min_length", "must not exceed max_length")
		}
		if f.Type == FieldShortText && v.Pattern != "" {
			if _, err := regexp.Compile(v.Pattern); err != nil || len(v.Pattern) > maxTextPattern || !utf8.ValidString(v.Pattern) {
				c.bad(loc+".pattern", "must be an RE2 expression of at most "+strconv.Itoa(maxTextPattern)+" bytes")
			} else {
				out.Pattern = v.Pattern
			}
		}
	case FieldMultiSelect:
		n := len(liveOptions(opts))
		out.MinSelect = c.intIn(loc+".min_select", v.MinSelect, 0, n)
		out.MaxSelect = c.intIn(loc+".max_select", v.MaxSelect, 1, n)
		if out.MinSelect != nil && out.MaxSelect != nil && *out.MinSelect > *out.MaxSelect {
			c.bad(loc+".min_select", "must not exceed max_select")
		}
	case FieldNumber:
		out.Integer = v.Integer
		out.Min = c.bound(loc+".min", v.Min, v.Integer)
		out.Max = c.bound(loc+".max", v.Max, v.Integer)
		if out.Min != nil && out.Max != nil && *out.Min > *out.Max {
			c.bad(loc+".min", "must not exceed max")
		}
	case FieldDate:
		from, okFrom := c.date(loc+".min_date", v.MinDate)
		to, okTo := c.date(loc+".max_date", v.MaxDate)
		if okFrom && okTo && v.MinDate != "" && v.MaxDate != "" && from.After(to) {
			c.bad(loc+".min_date", "must not be after max_date")
		}
		out.MinDate, out.MaxDate = v.MinDate, v.MaxDate
	case FieldURL:
		out.AllowedHosts = c.hosts(loc+".allowed_hosts", v.AllowedHosts)
	}
	if len(out.used()) == 0 {
		return nil
	}
	return &out
}

func (c *schemaChecker) intIn(loc string, p *int, lo, hi int) *int {
	if p == nil {
		return nil
	}
	if *p < lo || *p > hi {
		c.bad(loc, "must be between "+strconv.Itoa(lo)+" and "+strconv.Itoa(hi))
		return nil
	}
	v := *p
	return &v
}

func (c *schemaChecker) bound(loc string, p *float64, integer bool) *float64 {
	if p == nil {
		return nil
	}
	x := *p
	switch {
	case math.IsNaN(x) || math.Abs(x) > maxNumberAbs:
		c.bad(loc, "must be between -1e12 and 1e12")
		return nil
	case integer && x != math.Trunc(x):
		c.bad(loc, "must be a whole number for an integer field")
		return nil
	}
	return &x
}

func (c *schemaChecker) date(loc, s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		c.bad(loc, "must be a YYYY-MM-DD date")
		return time.Time{}, false
	}
	return t, true
}

func (c *schemaChecker) hosts(loc string, hs []string) []string {
	if len(hs) > maxAllowedHosts {
		c.bad(loc, "must list at most "+strconv.Itoa(maxAllowedHosts)+" hosts")
		return nil
	}
	var out []string
	for i, h := range hs {
		if !hostRe.MatchString(h) || len(h) > 253 {
			c.bad(loc+"["+strconv.Itoa(i)+"]", "must be a lower-case host name such as linkedin.com")
			continue
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	return out
}
