package talent

import (
	"errors"

	"koro/internal/platform/problem"
)

// Errors of the talent module. BE-047 has no HTTP surface: the
// neutralisation rules are plain sentinels that BE-052 (the auditor
// endpoints, 04 §T) maps onto its 409 problem. They never carry a talent
// value.
var (
	// ErrNotFound: the neutralisation, the signal definition or the config
	// version does not exist.
	ErrNotFound = problem.ErrNotFound

	// ErrObjectionPermanent: an objection neutralisation (KVKK md. 11/1-g)
	// cannot be lifted by staff (04 §T liftTalentNeutralization: 409).
	ErrObjectionPermanent = errors.New("talent: an objection neutralisation is permanent")
	// ErrOptOutStudentOwned: an opt-out is lifted only by the student's
	// own privacy setting (activity_ranking_choice), never by staff.
	ErrOptOutStudentOwned = errors.New("talent: an opt-out is lifted by the student's privacy setting only")
	// ErrNotActive: the neutralisation was lifted already.
	ErrNotActive = errors.New("talent: the neutralisation is not active")
	// ErrInvalidReason: a neutralisation reason outside 02 §15.8, or an
	// objection without its data subject request.
	ErrInvalidReason = errors.New("talent: invalid neutralisation reason")

	// errNoTalentTx: a store method ran outside a talent transaction (no
	// SET LOCAL ROLE app_talent). Always a programming error.
	errNoTalentTx = errors.New("talent: store used outside a talent transaction (SET LOCAL ROLE app_talent)")
	// errForeignTx: a talent transaction was asked for inside another
	// module's transaction. SET LOCAL ROLE would leak into that
	// transaction, and app_talent cannot write any public table, so talent
	// never joins one.
	errForeignTx = errors.New("talent: a talent transaction cannot join another module's transaction")
)
