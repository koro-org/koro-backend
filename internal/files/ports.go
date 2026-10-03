package files

import (
	"context"
	"uuid"

	"koro/internal/audit"
)

// AuditLogger is what files needs from audit (01 §4.1): every write of a
// file row (upload start and completion, attach, delete, orphan purge) is
// logged in the transaction of the write.
type AuditLogger interface {
	// Log appends an audit event in the transaction carried by ctx.
	//
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// RejectionNotice tells an uploader that one of their files was taken out
// of use after the upload. It carries no file name or content: the
// notification text is fixed per reason.
type RejectionNotice struct {
	FileID  uuid.UUID
	UserID  uuid.UUID // the uploader
	Purpose Purpose
	Reason  RejectReason
}

// OwnerNotifier informs the uploader of a rejected file (05 §14.7
// "başarısızsa: sil + kullanıcıya bildir"; T-163). notify is L3, so the
// composition root adapts notify.Sender to it (cmd/koro/wire_files.go). It
// runs in the worker (file.rejected → files.notify_rejected) and must be
// idempotent per FileID: the event is delivered at least once.
type OwnerNotifier interface {
	NotifyFileRejected(ctx context.Context, n RejectionNotice) error
}
