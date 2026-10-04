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

// Registration DTO mappings (BE-028).
var (
	ToSelfDTO = toSelfDTO
	ToOrgDTO  = toOrgDTO
)

// RegistrationTemplates are the in-app templates of the registration
// notices (BE-028).
var RegistrationTemplates = []string{templateSubmitted, templateReceived, templateWaitlisted, templateRejected, templateCancelled}
