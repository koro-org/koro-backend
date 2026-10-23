package kvkk

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"uuid"

	"koro/internal/platform/privacy"
)

// destructionRecorder is privacy.DestructionRecorder (T-318; BE-083): the
// destruction record of the module jobs that destroy on their own clock
// (events.purge_exports: an export file at its expiry). The sweepers
// return a privacy.SweepResult and kvkk.retention_sweep records their run;
// kvkk.purge_exports writes its own rows. A module reaches it only through
// hooks.destruction (cmd/koro), so no module imports kvkk.
type destructionRecorder struct{ s *service }

var _ privacy.DestructionRecorder = destructionRecorder{}

// errBadDestruction is a malformed record: a wiring or programming error of
// the calling module, never a user input. The destruction rolls back with
// it (a destruction must not go unrecorded).
var errBadDestruction = errors.New("kvkk: malformed destruction record")

// Record implements privacy.DestructionRecorder: one destruction_logs row
// in the caller's transaction (ctx). The policy must exist and belong to
// d.Module (a module records only its own policies); an empty Target is
// the policy's; zero times are now. Details are copied (no personal data:
// the caller's contract, 02 §16.3).
func (r destructionRecorder) Record(ctx context.Context, d privacy.Destruction) error {
	switch {
	case d.PolicyCode == "" || d.Module == "":
		return fmt.Errorf("%w: policy code and module are required", errBadDestruction)
	case d.RunID == uuid.Nil():
		return fmt.Errorf("%w: run id is required (policy %s)", errBadDestruction, d.PolicyCode)
	case d.Count < 0:
		return fmt.Errorf("%w: negative count (policy %s)", errBadDestruction, d.PolicyCode)
	case !slices.Contains(policyMethods, d.Method):
		return fmt.Errorf("%w: unknown method %q (policy %s)", errBadDestruction, d.Method, d.PolicyCode)
	}
	p, err := r.s.d.Records.Policy(ctx, d.PolicyCode)
	switch {
	case errors.Is(err, ErrNotFound):
		return fmt.Errorf("%w: unknown retention policy %s", errBadDestruction, d.PolicyCode)
	case err != nil:
		return err
	case p.OwnerModule != d.Module:
		return fmt.Errorf("%w: policy %s belongs to %s, not %s", errBadDestruction, d.PolicyCode, p.OwnerModule, d.Module)
	}
	now := r.s.now()
	started := d.StartedAt
	if started.IsZero() {
		started = now
	}
	finished := d.FinishedAt
	if finished.IsZero() {
		finished = now
	}
	cutoff := d.PeriodCutoff
	if cutoff.IsZero() {
		cutoff = started
	}
	details := maps.Clone(d.Details)
	if details == nil {
		details = map[string]any{}
	}
	return r.s.d.Records.InsertDestructionLog(ctx, DestructionLog{ID: uuid.NewV7(), PolicyCode: d.PolicyCode, RunID: d.RunID,
		Module: d.Module, Target: cmp.Or(d.Target, p.Target), Method: d.Method, RecordCount: d.Count,
		PeriodCutoff: cutoff.UTC(), StartedAt: started.UTC(), FinishedAt: finished.UTC(), Details: details})
}
