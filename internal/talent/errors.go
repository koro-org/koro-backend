package talent

import (
	"errors"

	"koro/internal/platform/problem"
)

// Errors of the talent module. The problems are the answers of the
// talent_auditor endpoints (04 §T, BE-052); none ever carries a talent
// value.
var (
	// ErrNotFound: the subject, the neutralisation, the ledger row, the
	// signal definition or the config version does not exist — and every
	// talent_auditor endpoint while talent.auditor_api_enabled is false
	// (03 §5.12 k-flag: 404 for everyone, before any authorisation).
	ErrNotFound = problem.ErrNotFound

	// ErrObjectionPermanent: an objection neutralisation (KVKK md. 11/1-g)
	// cannot be lifted by staff (04 §T liftTalentNeutralization: 409).
	ErrObjectionPermanent = problem.New(problem.Conflict, "talent.objection_permanent",
		"An objection neutralisation cannot be lifted")
	// ErrOptOutStudentOwned: an opt-out is lifted only by the student's
	// own privacy setting (activity_ranking_choice), never by staff.
	ErrOptOutStudentOwned = problem.New(problem.Conflict, "talent.opt_out_student_owned",
		"An opt-out is lifted by the student's privacy setting only")
	// ErrNotActive: the neutralisation was lifted already.
	ErrNotActive = problem.New(problem.Conflict, "talent.neutralization_not_active", "The neutralisation is not active")
	// ErrAlreadyNeutralized: the subject has an active auditor
	// neutralisation (one per user and reason, neutralizations_active_uq).
	ErrAlreadyNeutralized = problem.New(problem.Conflict, "talent.already_neutralized",
		"The user already has an active auditor neutralisation")
	// ErrAlreadyReversed: the ledger row has its reversal already
	// (point_events_reversal_uq: one reversal per row).
	ErrAlreadyReversed = problem.New(problem.Conflict, "talent.already_reversed", "The point event is already reversed")
	// ErrNotReversible: a reversal row is never reversed itself (the
	// ledger is corrected forwards only).
	ErrNotReversible = problem.New(problem.Conflict, "talent.not_reversible", "A reversal cannot be reversed")
	// ErrUnavailable: the re-authentication port is not wired; an R
	// operation fails closed (503).
	ErrUnavailable = problem.ErrUnavailable

	// ErrInvalidReason: a neutralisation reason outside 02 §15.8, or an
	// objection without its data subject request (system calls only).
	ErrInvalidReason = errors.New("talent: invalid neutralisation reason")
	// errInvalidParams: the params of the config version in force do not
	// validate; the nightly run fails without retry (a new version fixes it).
	errInvalidParams = errors.New("talent: invalid config params")

	// errNoTalentTx: a store method ran outside a talent transaction (no
	// SET LOCAL ROLE app_talent). Always a programming error.
	errNoTalentTx = errors.New("talent: store used outside a talent transaction (SET LOCAL ROLE app_talent)")
	// errForeignTx: a talent transaction was asked for inside another
	// module's transaction. SET LOCAL ROLE would leak into that
	// transaction, and app_talent cannot write any public table, so talent
	// never joins one.
	errForeignTx = errors.New("talent: a talent transaction cannot join another module's transaction")
)
