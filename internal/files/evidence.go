package files

import (
	"context"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/problem"
)

// Rejected evidence of a financial or legal record (T-393, Q-37; BE-154).
// The owning module keeps the reference of a rejected file (BE-151: the row
// is a financial record and its audit trail), shows the file's state on the
// record and lets the role that could upload it attach a new file in its
// place. The old file row stays attached to the record: with its purpose,
// checksum, uploader and status it is the history of the evidence and lives
// as long as the record (files:financial, 10 years); its object is gone.

// ErrNotRejected: the record's file was not rejected (it is clean or still
// being scanned): only a rejected file may be replaced.
var ErrNotRejected = problem.New(problem.Conflict, "files.not_rejected", "Only a rejected file can be replaced")

// State is what an owning module shows of an attached file (the
// `*_file_state` members of its views).
type State string

// File states.
const (
	// StateClean: the file is served (scanned clean, or skipped while no
	// scanner is required).
	StateClean State = "clean"
	// StatePending: the upload or its scan is not finished (or a skipped
	// file waits for the required scanner).
	StatePending State = "pending"
	// StateRejected: the file was taken out of use after its upload
	// (infected or unprocessable); its object is deleted. The record's
	// evidence must be uploaded again.
	StateRejected State = "rejected"
)

// StateValues returns every State (the OpenAPI enum of a state member,
// op.SetEnum).
func StateValues() []State { return []State{StateClean, StatePending, StateRejected} }

// Rejected reports whether the file was taken out of use after its upload:
// it could not be processed (status rejected, BE-059) or the scan found
// malware (BE-048; an attached row stays, infected).
func (f FileMeta) Rejected() bool {
	return f.Status == StatusRejected || f.ScanStatus == ScanInfected
}

// State returns the file's State under the rule of PresignGet, with
// scanRequired = FILES_SCAN_REQUIRED (a skipped scan is clean only while no
// scanner is required).
func (f FileMeta) State(scanRequired bool) State {
	switch {
	case f.Rejected():
		return StateRejected
	case f.Status == StatusActive && (f.ScanStatus == ScanClean || f.ScanStatus == ScanSkipped && !scanRequired):
		return StateClean
	}
	return StatePending
}

// RejectReason returns why a rejected file was taken out of use ("" for a
// file that was not).
func (f FileMeta) RejectReason() RejectReason {
	switch {
	case f.ScanStatus == ScanInfected:
		return RejectInfected
	case f.Status == StatusRejected:
		return RejectUnprocessable
	}
	return ""
}

// CheckReplace checks that cur, the file a record holds now, may be
// replaced: it was rejected (409 ErrNotRejected otherwise).
func CheckReplace(cur FileMeta) error {
	if !cur.Rejected() {
		return ErrNotRejected
	}
	return nil
}

// Replacer is what Replace needs of files: the owning module's Files port
// (its Attach is the module's declared sync write, 01 §6.3).
type Replacer interface {
	Get(ctx context.Context, fileID uuid.UUID) (FileMeta, error)
	Attach(ctx context.Context, a authz.Actor, fileID uuid.UUID, owner Owner) error
}

// Replace is the files half of replacing a record's rejected evidence, in
// the owning module's transaction after it locked the record: cur, the
// file the locked record holds, must be rejected (409 ErrNotRejected; a
// replacement that won the race left a pending file) and next is attached
// to owner (Attach: of owner's purpose, completed, not infected, held by no
// other record). The module then writes next into its column (the database
// guard, koro_evidence_file_guard, lets only a rejected file go) and audits
// the returned ReplaceMeta. The old row stays attached to the record.
func Replace(ctx context.Context, f Replacer, a authz.Actor, cur, next uuid.UUID, owner Owner) (map[string]any, error) {
	m, err := f.Get(ctx, cur)
	if err != nil {
		return nil, err
	}
	if err := CheckReplace(m); err != nil {
		return nil, err
	}
	if err := f.Attach(ctx, a, next, owner); err != nil {
		return nil, err
	}
	return ReplaceMeta(m, next), nil
}

// StateOf returns the state of file id in states ("" without a file, or
// for a file states does not know).
func StateOf(states map[uuid.UUID]State, id *uuid.UUID) State {
	if id == nil {
		return ""
	}
	return states[*id]
}

// IDs returns the set file IDs of ids, each once (the argument of States).
func IDs(ids ...*uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if id != nil && !seen[*id] {
			seen[*id] = true
			out = append(out, *id)
		}
	}
	return out
}

// ReplaceMeta is the audit metadata of a replaced rejected file (IDs and
// codes only): the old file stays the record's history.
func ReplaceMeta(cur FileMeta, next uuid.UUID) map[string]any {
	return map[string]any{
		"operation":        "replace_rejected_file",
		"replaced_file_id": cur.ID.String(),
		"replaced_reason":  string(cur.RejectReason()),
		"file_id":          next.String(),
	}
}

// state is the State of f under the service's scan rule: clean only when
// it is served (servable: never a quarantined object).
func (s *service) state(f File) State {
	st := f.meta().State(s.d.ScanRequired)
	if st == StateClean && s.servable(f) != nil {
		return StatePending
	}
	return st
}

// States implements Files.
func (s *service) States(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]State, error) {
	out := make(map[uuid.UUID]State, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.d.Store.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, f := range rows {
		out[f.ID] = s.state(f)
	}
	return out, nil
}
