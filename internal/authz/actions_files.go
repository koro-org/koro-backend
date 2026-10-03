// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the files module (03 §5.20), in table order.
const (
	FilesUploadCreate   Action = "files.upload.create"
	FilesUploadComplete Action = "files.upload.complete"
	FilesFileDeleteOwn  Action = "files.file.delete_own"
)

// System-only actions of the files module (03 §3.7): only SystemActor may perform them.
const (
	FilesOrphanPurge Action = "files.orphan.purge"
)

// filesActions lists the HTTP actions of files in 03 §5.20 order.
var filesActions = []Action{
	FilesUploadCreate,
	FilesUploadComplete,
	FilesFileDeleteOwn,
}
