package talent

import "log/slog"

// Band is the ranking band of a student (01 §4.11): 0 is neutral (no
// score, a new or neutralised user, a low score: low is never a penalty),
// 1..3 lift the ranking (ADR-0015: only upwards). It flows only from talent
// to discovery, which multiplies the relevance of a result with it.
//
// The talent value never enters a DTO, a log, an event or an API response
// (01 §0-7, 03 §3.4 Y1). Band defends that rule itself: it cannot be
// serialised to JSON or text (MarshalJSON and MarshalText panic, so a Band
// put into a response body or a map key fails loudly in the first test),
// and it prints and logs redacted. The only deliberate exception is the
// auditor report (Report.JSON), which converts it explicitly.
type Band uint8

// Bands.
const (
	BandNeutral Band = 0
	BandMax     Band = 3
)

// bandPanic is the message of a Band that reached an encoder.
const bandPanic = "talent: a Band must never be serialised (01 §0-7: no talent value in any DTO, log or event)"

// MarshalJSON panics: a Band must not reach a response or an event.
func (Band) MarshalJSON() ([]byte, error) { panic(bandPanic) }

// MarshalText panics: a Band must not become a map key or a text value.
func (Band) MarshalText() ([]byte, error) { panic(bandPanic) }

// String hides the value from %v and %s (logs, error messages).
func (Band) String() string { return "talent.Band(redacted)" }

// GoString hides the value from %#v.
func (Band) GoString() string { return "talent.Band(redacted)" }

// LogValue hides the value from slog.
func (Band) LogValue() slog.Value { return slog.StringValue("redacted") }

// bandOf maps a talent.scores.band value (0..3 by its CHECK) onto a Band;
// anything else is neutral.
func bandOf(v int16) Band { return bandOfInt(int(v)) }

// bandOfInt maps 1..3 onto a Band; anything else is neutral.
func bandOfInt(v int) Band {
	switch v {
	case 1:
		return 1
	case 2:
		return 2
	case 3:
		return BandMax
	}
	return BandNeutral
}
