package files

import (
	"errors"
	"uuid"
)

// The answers of CheckOrgUpload the caller words itself, as a 422 on the
// field that named the file: an unknown file, a file of another purpose
// and another org's file read alike, so a request never probes another
// org's uploads; an upload that has not completed is the caller's 422 too.
var (
	ErrNotOrgUpload    = errors.New("files: not an upload of the purpose by the org")
	ErrUploadNotActive = errors.New("files: the upload has not completed")
)

// CheckOrgUpload checks, before the owner's transaction, that f can become
// owner's file: an upload of owner.Purpose by org (06 H-38: the caller
// takes org from the owning row, never from the request), completed, not
// found infected, held by no other record (a retry for the same owner
// passes). Attach repeats the purpose, status and attachment checks under
// the file's lock. ErrRejected, ErrNotClean and ErrAlreadyAttached are the
// files errors (409); ErrNotOrgUpload and ErrUploadNotActive are the
// caller's to word.
func (f FileMeta) CheckOrgUpload(org uuid.UUID, owner Owner) error {
	switch {
	case f.Purpose != owner.Purpose, f.OrgID == nil, *f.OrgID != org:
		return ErrNotOrgUpload
	case f.Status == StatusRejected:
		return ErrRejected
	case f.Status != StatusActive:
		return ErrUploadNotActive
	case f.ScanStatus == ScanInfected:
		return ErrNotClean
	case f.AttachedModule != "" && (f.AttachedModule != owner.Module || f.AttachedEntityID != owner.EntityID):
		return ErrAlreadyAttached
	}
	return nil
}
