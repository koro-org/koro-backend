package events

// Test hooks (package events_test): the DTO mapping of the handlers and the
// lint engine.
var (
	ToEventDTO = toEventDTO
	ToCardDTO  = toCardDTO
	ToFormDTO  = toFormDTO
)

// LintSchema runs rules (the active ones) over s.
func LintSchema(s FormSchema, rules []LintRule) LintReport {
	compiled, err := compileRules(rules)
	if err != nil {
		panic(err)
	}
	return lintSchema(s, compiled)
}
