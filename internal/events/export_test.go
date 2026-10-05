package events

import "uuid"

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

// BE-031 hooks: the QR payload parser (fuzzed), the manual code reader, the
// calendar file and the batch size of the jobs and consumers.
var (
	ParseQRPayload      = parseQRPayload
	NormalizeManualCode = normalizeManualCode
	ToCheckinResultDTO  = toCheckinResultDTO
	ToTicketDTO         = toTicketDTO
)

// ICS renders the calendar file of a registration (the ticket mails).
func ICS(method string, e Event, regID uuid.UUID, seq int, attendee string) []byte {
	return buildICS(icsEvent{Method: method, EventID: e.ID, RegistrationID: regID, Sequence: seq, Stamp: e.UpdatedAt,
		Start: e.StartsAt, End: e.EndsAt, Summary: e.Title, Location: locationText(e), OrganizerName: "Org",
		OrganizerEmail: "noreply@koro.test", AttendeeEmail: attendee})
}

// ICSSequence is the SEQUENCE of an event's current state.
func ICSSequence(e Event) int { return icsSequence(e) }

// SetBatch sets the rows per transaction of the module's jobs and
// consumers (tests of the batching).
func SetBatch(m Module, n int) { m.svc.batch = n }

// BE-031 template IDs (the e-mails must exist in every locale).
var TicketTemplates = []string{templateTicketIssued, templateTicketReissued, templateScheduleChanged, templateCancelledEmail}
