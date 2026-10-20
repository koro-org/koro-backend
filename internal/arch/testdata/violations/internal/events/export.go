// R operations of the miniature events module (TestArch_ReauthFailureAudited):
// the proof check of each one must return its error through
// audit.ReauthFailed (03 §6.2, BE-075). Parsed, never compiled.
package events

import (
	"context"

	"koro/internal/audit"
	"koro/internal/platform/httpx/op"
)

var exportMeta = op.Meta{Action: "events.export.create", Reauth: true}

// CreateExport returns the refusal as it came: no denied record.
func (s *service) CreateExport(ctx context.Context, a actor, proof reauthProof) error {
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		return err
	}
	return s.export(ctx, kind)
}

// ExportRouted is the rule's shape: fine.
func (s *service) ExportRouted(ctx context.Context, a actor, proof reauthProof) error {
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		denied := audit.Event{Action: "events.export.create", ResourceType: "event"}.By(a)
		return audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	return s.export(ctx, kind)
}

// ExportInit checks the proof in the if statement's init: fine.
func (s *service) ExportInit(ctx context.Context, a actor, proof reauthProof) error {
	if _, err := s.d.Reauth.Verify(ctx, a.UserID, proof); err != nil {
		return audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, audit.Event{Action: "events.export.create"}, err)
	}
	return nil
}

// ExportDirect hands the refusal to its caller unaudited.
func (s *service) ExportDirect(ctx context.Context, a actor, proof reauthProof) (string, error) {
	return s.d.Reauth.Verify(ctx, a.UserID, proof)
}

// ExportPartly skips the record on one branch.
func (s *service) ExportPartly(ctx context.Context, a actor, proof reauthProof) error {
	_, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		if a.Admin {
			return err
		}
		return audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, audit.Event{Action: "events.export.create"}, err)
	}
	return nil
}

// ExportOtherErr audits another error than the proof's.
func (s *service) ExportOtherErr(ctx context.Context, a actor, proof reauthProof) error {
	_, verr := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if verr != nil {
		return audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, audit.Event{Action: "events.export.create"}, ctx.Err())
	}
	return nil
}

// ExportLater does something else between the check and its handling.
func (s *service) ExportLater(ctx context.Context, a actor, proof reauthProof) error {
	_, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	s.d.Log.Info("proof checked")
	if err != nil {
		return audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, audit.Event{Action: "events.export.create"}, err)
	}
	return nil
}

// ExportDropped drops the error.
func (s *service) ExportDropped(ctx context.Context, a actor, proof reauthProof) {
	_, _ = s.d.Reauth.Verify(ctx, a.UserID, proof)
}

// ExportClosure checks the proof in a closure and returns the refusal.
func (s *service) ExportClosure(ctx context.Context, a actor, proof reauthProof) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
		return err
	})
}
