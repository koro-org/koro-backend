package events

import (
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"uuid"

	"golang.org/x/text/unicode/norm"

	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// KVKK form lint (02 §11.4–11.5, R-003 §2.6; T-133: the classes of 02 win).
// Every active form_lint_rules pattern (RE2) runs over the folded texts of
// each field: its key (underscores as spaces), labels, help texts and
// option labels and values; the form description counts as a field with
// an empty key. A hit is (field, rule); the report groups the hits by
// class:
//
//   - blocked: the version cannot be published, there is no override (the
//     event_form_versions_blocked_chk CHECK says the same);
//   - warn_special: published only with a reasoned override; the version
//     then has has_sensitive_fields and its answers are encrypted and kept
//     short (BE-028);
//   - review_identifier: an identifier that needs a legal reason; published
//     only with a reasoned override too (BE-026 reading of 02 §11.4: the
//     report names the class, R-003 §2.6 asks for a justification).
//
// The texts are folded like koro_norm (Turkish lower case, accents and
// format characters removed), so the patterns are written in lower-case
// ASCII ("saglik", "kimlik").

// LintHit is one rule matching one field.
type LintHit struct {
	FieldKey      string    `json:"field_key" doc:"Key of the field; empty for the form description"`
	RuleID        uuid.UUID `json:"rule_id"`
	DescriptionTR string    `json:"description_tr" doc:"Why the rule exists (Turkish)"`
}

// LintReport is the lint result of a schema (event_form_versions.lint_report).
// The arrays are never null.
type LintReport struct {
	Blocked          []LintHit `json:"blocked"`
	WarnSpecial      []LintHit `json:"warn_special"`
	ReviewIdentifier []LintHit `json:"review_identifier"`
}

// NeedsOverride reports whether publication needs lint_override_reason.
func (r LintReport) NeedsOverride() bool {
	return len(r.WarnSpecial) > 0 || len(r.ReviewIdentifier) > 0
}

// SensitiveKeys are the keys of the warn_special fields: their answers go
// to answers_sensitive_enc (02 §11.6, BE-028).
func (r LintReport) SensitiveKeys() []string {
	var out []string
	for _, h := range r.WarnSpecial {
		if h.FieldKey != "" && !slices.Contains(out, h.FieldKey) {
			out = append(out, h.FieldKey)
		}
	}
	return out
}

// JSON returns the stored form of the report.
func (r LintReport) JSON() JSON {
	b, err := json.Marshal(r.nonNil())
	if err != nil {
		panic("events: lint report does not encode: " + err.Error())
	}
	return JSON(b)
}

func (r LintReport) nonNil() LintReport {
	for _, p := range []*[]LintHit{&r.Blocked, &r.WarnSpecial, &r.ReviewIdentifier} {
		if *p == nil {
			*p = []LintHit{}
		}
	}
	return r
}

// DecodeLintReport reads a stored report.
func DecodeLintReport(raw []byte) (LintReport, error) {
	var r LintReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return LintReport{}, fmt.Errorf("events: stored lint report: %w", err)
	}
	return r.nonNil(), nil
}

// compiledRule is an active rule with its expression.
type compiledRule struct {
	rule LintRule
	re   *regexp.Regexp
}

// compileRules compiles the active rules (class order, then the stored
// order). A stored rule that does not compile fails the lint (fail closed:
// a skipped blocked rule would let the field through); rules are checked
// when they are written (checkPattern), so this is a programmer error.
func compileRules(rules []LintRule) ([]compiledRule, error) {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		if !r.Active {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("events: lint rule %s does not compile: %w", r.ID, err)
		}
		out = append(out, compiledRule{rule: r, re: re})
	}
	rank := map[LintClass]int{LintBlocked: 0, LintWarnSpecial: 1, LintReviewIdentifier: 2}
	slices.SortStableFunc(out, func(a, b compiledRule) int { return rank[a.rule.Class] - rank[b.rule.Class] })
	return out, nil
}

// lintSchema runs the rules over the schema.
func lintSchema(s FormSchema, rules []compiledRule) LintReport {
	var rep LintReport
	type target struct {
		key   string
		texts []string
	}
	targets := []target{}
	if t := localTexts(s.Description, true); len(t) > 0 {
		targets = append(targets, target{key: "", texts: t})
	}
	for _, f := range s.Fields {
		t := []string{strings.ReplaceAll(f.Key, "_", " ")}
		t = append(t, localTexts(f.Label, false)...)
		t = append(t, localTexts(f.Help, true)...)
		for _, o := range f.Options {
			t = append(t, strings.NewReplacer("_", " ", "-", " ").Replace(o.Value))
			t = append(t, localTexts(o.Label, false)...)
		}
		targets = append(targets, target{key: f.Key, texts: t})
	}
	for _, tg := range targets {
		folded := make([]string, len(tg.texts))
		for i, t := range tg.texts {
			folded[i] = lintFold(t)
		}
		for _, r := range rules {
			if !slices.ContainsFunc(folded, r.re.MatchString) {
				continue
			}
			hit := LintHit{FieldKey: tg.key, RuleID: r.rule.ID, DescriptionTR: r.rule.DescriptionTR}
			switch r.rule.Class {
			case LintBlocked:
				rep.Blocked = append(rep.Blocked, hit)
			case LintWarnSpecial:
				rep.WarnSpecial = append(rep.WarnSpecial, hit)
			case LintReviewIdentifier:
				rep.ReviewIdentifier = append(rep.ReviewIdentifier, hit)
			}
		}
	}
	return rep.nonNil()
}

// htmlTag matches a tag of the sanitised HTML (well formed by then).
var htmlTag = regexp.MustCompile(`<[^>]*>`)

// localTexts returns the texts of every locale; HTML loses its tags and
// entities.
func localTexts(t LocalText, isHTML bool) []string {
	out := make([]string, 0, len(t))
	for _, k := range formLocales {
		v, ok := t[k]
		if !ok || v == "" {
			continue
		}
		if isHTML {
			v = html.UnescapeString(htmlTag.ReplaceAllString(v, " "))
		}
		out = append(out, v)
	}
	return out
}

// lintFold is the Go side of koro_norm (02 §0.3): Turkish lower case,
// accents removed (NFKD without combining marks, "ı" → "i"), and format
// characters (zero-width joiners and the like) dropped so that they cannot
// split a word past a rule.
func lintFold(s string) string {
	s = norm.NFKD.String(strings.ReplaceAll(textx.LowerTR(s), "ı", "i"))
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Limits of a lint rule (02 §11.5 form_lint_rules_pattern_chk).
const (
	maxLintPattern     = 500
	minLintDescription = 3
	maxLintDescription = 500
)

// checkPattern is the write-time check of a rule pattern: it must compile
// as RE2 (Go regexp; 422 otherwise) and must not match the empty text (it
// would flag every form).
func checkPattern(loc, p string) (string, error) {
	if p == "" || len(p) > maxLintPattern {
		return "", problem.Field(loc, "must be 1-500 characters")
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return "", problem.Field(loc, "must be a valid RE2 expression")
	}
	if re.MatchString("") {
		return "", problem.Field(loc, "must not match the empty text")
	}
	return p, nil
}
