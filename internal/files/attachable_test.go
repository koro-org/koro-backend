package files_test

import (
	"errors"
	"testing"
	"uuid"

	"koro/internal/files"
)

// TestFileMeta_CheckOrgUpload (BE-132): the shared pre-check of an org's
// evidence upload (orders' delivery evidence, payments' refund request
// evidence): another purpose, no org or another org read alike; a rejected,
// unfinished or infected upload and one held by another record are refused;
// a retry for the same owner passes.
func TestFileMeta_CheckOrgUpload(t *testing.T) {
	t.Parallel()
	org, other, row := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	owner := files.Owner{Module: "payments", EntityID: row, Purpose: files.PurposeRefundRequestEvidence}
	ok := files.FileMeta{ID: uuid.NewV7(), Purpose: files.PurposeRefundRequestEvidence, OrgID: &org,
		Status: files.StatusActive, ScanStatus: files.ScanPending, AttachedEntityID: uuid.Nil()}
	cases := []struct {
		name string
		edit func(f *files.FileMeta)
		want error
	}{
		{"a completed upload of the org", func(*files.FileMeta) {}, nil},
		{"held by the same owner (a retry)", func(f *files.FileMeta) { f.AttachedModule, f.AttachedEntityID = "payments", row }, nil},
		{"another purpose", func(f *files.FileMeta) { f.Purpose = files.PurposeDeliveryEvidence }, files.ErrNotOrgUpload},
		{"no org", func(f *files.FileMeta) { f.OrgID = nil }, files.ErrNotOrgUpload},
		{"another org", func(f *files.FileMeta) { f.OrgID = &other }, files.ErrNotOrgUpload},
		{"rejected", func(f *files.FileMeta) { f.Status = files.StatusRejected }, files.ErrRejected},
		{"still uploading", func(f *files.FileMeta) { f.Status = files.StatusUploading }, files.ErrUploadNotActive},
		{"infected", func(f *files.FileMeta) { f.ScanStatus = files.ScanInfected }, files.ErrNotClean},
		{"held by another record", func(f *files.FileMeta) { f.AttachedModule, f.AttachedEntityID = "payments", uuid.NewV7() },
			files.ErrAlreadyAttached},
	}
	for _, c := range cases {
		f := ok
		c.edit(&f)
		if err := f.CheckOrgUpload(org, owner); !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
			t.Errorf("%s: CheckOrgUpload = %v, want %v", c.name, err, c.want)
		}
	}
}
