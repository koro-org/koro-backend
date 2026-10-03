package files

import "koro/internal/platform/problem"

// Errors of the files module (05 §4.2; 04 §2.20). Field errors name the
// offending member, never a file name or content.
var (
	// ErrInvalidUpload: the upload request breaks the purpose's allow-list
	// (purpose, MIME type, file name or org rules).
	ErrInvalidUpload = problem.New(problem.Invalid, "files.invalid_upload", "Upload request is invalid")
	// ErrTooLarge: the declared or actual size exceeds the purpose limit.
	ErrTooLarge = problem.New(problem.Invalid, "files.too_large", "File is too large for its purpose")

	// ErrObjectMissing: completeUpload before the object was uploaded.
	ErrObjectMissing = problem.New(problem.Invalid, "files.object_missing", "Uploaded object not found")
	// ErrSizeMismatch: the uploaded object's size differs from the declared one.
	ErrSizeMismatch = problem.New(problem.Invalid, "files.size_mismatch", "Uploaded size does not match the declared size")
	// ErrChecksumMismatch: the object's SHA-256 differs from the client's.
	ErrChecksumMismatch = problem.New(problem.Invalid, "files.checksum_mismatch", "SHA-256 checksum does not match")
	// ErrContentMismatch: the magic bytes are not an allowed type of the
	// purpose or differ from the declared content type.
	ErrContentMismatch = problem.New(problem.Invalid, "files.content_mismatch", "File content does not match an allowed type")

	// ErrNotUploading: completeUpload on a file that is no longer uploading
	// (and not a retry of the same completion).
	ErrNotUploading = problem.New(problem.Conflict, "files.not_uploading", "File upload is already completed")
	// ErrNotActive: attaching or downloading a file whose upload is not complete.
	ErrNotActive = problem.New(problem.Conflict, "files.not_active", "File upload is not completed")
	// ErrNotClean: the file is not cleared for use (04 §1.14: scan pending,
	// infected, or skipped while FILES_SCAN_REQUIRED=true).
	ErrNotClean = problem.New(problem.Conflict, "files.not_clean", "File is not cleared by the virus scan")
	// ErrAttached: deleteMyFile on an attached file (the owning module deletes it).
	ErrAttached = problem.New(problem.Conflict, "files.attached", "File is in use; delete it where it is used")
	// ErrAlreadyAttached: Attach to another owner than the current one.
	ErrAlreadyAttached = problem.New(problem.Conflict, "files.already_attached", "File is attached to another record")
	// ErrPurposeMismatch: Attach with an owner expecting another purpose.
	ErrPurposeMismatch = problem.New(problem.Invalid, "files.purpose_mismatch", "File purpose does not fit this use")
	// ErrReferenced: Delete of a file another table still references (FK).
	ErrReferenced = problem.New(problem.Conflict, "files.referenced", "File is still referenced")

	// ErrStorageUnavailable: the object storage did not answer.
	ErrStorageUnavailable = problem.New(problem.Unavailable, "files.storage_unavailable", "File storage is unavailable")
	// ErrNotFound: the file does not exist (or is not visible).
	ErrNotFound = problem.ErrNotFound
)
