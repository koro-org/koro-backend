package files

import (
	"bytes"
	"time"
	"uuid"
)

// Status is files.status (02 §3.1).
type Status string

// File statuses.
const (
	// StatusUploading: the row exists, the browser may not have uploaded
	// yet; the object (if any) is in the quarantine bucket and unverified.
	StatusUploading Status = "uploading"
	// StatusActive: size, SHA-256 and magic bytes are verified.
	StatusActive Status = "active"
	// StatusRejected: the worker could not remove the metadata (corrupt,
	// oversized or unsupported content, or the object changed after the
	// completion). The object is deleted and never served; an unattached
	// row is removed by orphan cleanup (BE-059).
	StatusRejected Status = "rejected"
)

// ScanStatus is files.scan_status (02 §3.1).
type ScanStatus string

// Scan statuses.
const (
	ScanPending  ScanStatus = "pending"  // not scanned yet (object in quarantine)
	ScanClean    ScanStatus = "clean"    // scanned, no finding (or server-generated content)
	ScanInfected ScanStatus = "infected" // scanned, malware found: never served
	ScanSkipped  ScanStatus = "skipped"  // no real scanner (NoopScanner, until BE-048)
)

// File is one row of the files table (02 §3.1).
type File struct {
	ID               uuid.UUID  `gorm:"type:uuid;primaryKey"`
	Purpose          Purpose    `gorm:"column:purpose"`
	OwnerUserID      *uuid.UUID `gorm:"type:uuid;column:owner_user_id"`
	OrgID            *uuid.UUID `gorm:"type:uuid;column:org_id"`
	AttachedModule   *string    `gorm:"column:attached_module"`
	AttachedEntityID *uuid.UUID `gorm:"type:uuid;column:attached_entity_id"`
	AttachedAt       *time.Time `gorm:"column:attached_at"`
	Bucket           string     `gorm:"column:bucket"`
	StorageKey       string     `gorm:"column:storage_key"`
	OriginalName     *string    `gorm:"column:original_name"`
	DeclaredMIME     string     `gorm:"column:declared_mime"`
	DetectedMIME     *string    `gorm:"column:detected_mime"`
	SizeBytes        *int64     `gorm:"column:size_bytes"`
	SHA256           []byte     `gorm:"column:sha256"`
	Status           Status     `gorm:"column:status"`
	ScanStatus       ScanStatus `gorm:"column:scan_status"`
	ScannedAt        *time.Time `gorm:"column:scanned_at"`
	UploadedBy       *uuid.UUID `gorm:"type:uuid;column:uploaded_by"`
	CreatedAt        time.Time  `gorm:"column:created_at"`
	UpdatedAt        time.Time  `gorm:"column:updated_at"`
}

// TableName implements gorm's Tabler.
func (File) TableName() string { return "files" }

// Attached reports whether an owning module has attached the file.
func (f File) Attached() bool { return f.AttachedModule != nil }

func (f File) clone() File {
	c := f
	c.SHA256 = bytes.Clone(f.SHA256)
	return c
}

// FileMeta is the read model other modules get (01 §4.2 Get). It never
// carries the storage location.
type FileMeta struct {
	ID               uuid.UUID
	Purpose          Purpose
	OwnerUserID      *uuid.UUID
	OrgID            *uuid.UUID
	UploadedBy       *uuid.UUID
	AttachedModule   string    // "" when not attached
	AttachedEntityID uuid.UUID // uuid.Nil() when not attached
	OriginalName     string    // sanitised display name ("" when none)
	MIME             string    // detected MIME once active, else the declared one
	SizeBytes        int64
	SHA256           []byte
	Status           Status
	ScanStatus       ScanStatus
	CreatedAt        time.Time
}

func (f File) meta() FileMeta {
	m := FileMeta{
		ID: f.ID, Purpose: f.Purpose, OwnerUserID: f.OwnerUserID, OrgID: f.OrgID, UploadedBy: f.UploadedBy,
		MIME: f.DeclaredMIME, SHA256: bytes.Clone(f.SHA256), Status: f.Status, ScanStatus: f.ScanStatus,
		CreatedAt: f.CreatedAt, AttachedEntityID: uuid.Nil(),
	}
	if f.AttachedModule != nil && f.AttachedEntityID != nil {
		m.AttachedModule, m.AttachedEntityID = *f.AttachedModule, *f.AttachedEntityID
	}
	if f.OriginalName != nil {
		m.OriginalName = *f.OriginalName
	}
	if f.DetectedMIME != nil {
		m.MIME = *f.DetectedMIME
	}
	if f.SizeBytes != nil {
		m.SizeBytes = *f.SizeBytes
	}
	return m
}

// UploadRequest starts a browser upload (04 §1.14 createUpload).
type UploadRequest struct {
	Purpose     Purpose
	Filename    string // display name only; never part of the storage key
	ContentType string // declared; verified by magic bytes on completion
	SizeBytes   int64
	OrgID       *uuid.UUID
}

// Upload is the answer of CreateUpload: where and how to PUT the object.
type Upload struct {
	FileID        uuid.UUID         `json:"file_id"`
	UploadURL     string            `json:"upload_url"`
	UploadHeaders map[string]string `json:"upload_headers"`
	ExpiresAt     time.Time         `json:"expires_at"`
}

// GeneratedFile is a server-generated file (01 §4.2 PutGenerated: exports,
// PDFs). Its content is trusted: it is stored directly in the main bucket.
type GeneratedFile struct {
	Purpose     Purpose
	OwnerUserID *uuid.UUID // data subject, when the content is personal
	OrgID       *uuid.UUID
	Name        string // display name
	ContentType string
	Content     []byte
}

// Owner identifies the record that keeps a file alive (01 §4.2 Attach).
type Owner struct {
	Module   string    // owning module, e.g. "students"
	EntityID uuid.UUID // owning row
	// Purpose is the purpose the owner expects: attaching a file of
	// another purpose (a CV as an avatar) is refused.
	Purpose Purpose
}

// Download is a short-lived download URL (04 §1.14: {url, expires_at}).
type Download struct {
	URL       string
	ExpiresAt time.Time
}
