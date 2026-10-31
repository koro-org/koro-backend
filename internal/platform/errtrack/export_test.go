package errtrack

// Internals exported for the black-box tests (OPS-014).
var (
	ScrubEvent = scrubEvent
	ToSentry   = toSentry
)
