package events

import (
	"time"
	"uuid"

	"koro/internal/platform/db"
)

// ExportStatus is event_data_exports.status (02 §11.9).
type ExportStatus string

// Export statuses: queued until the worker stores the file (ready) or gives
// up (failed); a ready export's file is deleted at expires_at (purged).
const (
	ExportQueued ExportStatus = "queued"
	ExportReady  ExportStatus = "ready"
	ExportFailed ExportStatus = "failed"
	ExportPurged ExportStatus = "purged"
)

// ExportFormat is event_data_exports.format (migration 000126): the file
// type the worker writes.
type ExportFormat string

// Export formats (files purpose event_export: text/csv, xlsx).
const (
	FormatCSV  ExportFormat = "csv"
	FormatXLSX ExportFormat = "xlsx"
)

// Valid reports whether f is a format of the CHECK constraint.
func (f ExportFormat) Valid() bool { return f == FormatCSV || f == FormatXLSX }

// Export is one row of event_data_exports (02 §11.9): who exported which
// fields of an event's registrations, why, from where, and the file while
// it lives. event_id and org_id come from the events row (06 H-38).
type Export struct {
	ID                uuid.UUID    `gorm:"type:uuid;column:id;primaryKey"`
	EventID           uuid.UUID    `gorm:"type:uuid;column:event_id"`
	OrgID             uuid.UUID    `gorm:"type:uuid;column:org_id"`
	RequestedBy       uuid.UUID    `gorm:"type:uuid;column:requested_by"`
	Reason            string       `gorm:"column:reason"`
	Fields            db.TextArray `gorm:"column:fields;type:text[]"`
	Format            ExportFormat `gorm:"column:format"`
	IncludeSensitive  bool         `gorm:"column:include_sensitive"`
	Status            ExportStatus `gorm:"column:status"`
	RowCount          *int         `gorm:"column:row_count"`
	FileID            *uuid.UUID   `gorm:"type:uuid;column:file_id"`
	FileSHA256        []byte       `gorm:"column:file_sha256"`
	IP                *string      `gorm:"column:ip"`
	ExpiresAt         time.Time    `gorm:"column:expires_at"`
	FirstDownloadedAt *time.Time   `gorm:"column:first_downloaded_at"`
	DownloadCount     int          `gorm:"column:download_count"`
	PurgedAt          *time.Time   `gorm:"column:purged_at"`
	CreatedAt         time.Time    `gorm:"column:created_at;->"`
	UpdatedAt         time.Time    `gorm:"column:updated_at;->"`
}

// TableName implements gorm's Tabler.
func (Export) TableName() string { return "event_data_exports" }

// ExportInput is the body of createEventExport (04 §2.6): the reason
// (10–500 characters), the fields (the allow-list, 1–40), the file format
// and include_sensitive, which v1 refuses (T-37).
type ExportInput struct {
	Reason           string
	Fields           []string
	Format           ExportFormat // "" → xlsx
	IncludeSensitive bool
}

// ExportPage is a page of listEventExports, newest first.
type ExportPage struct {
	Items []Export
	Next  *After // created_at and id of the last row
}

// ExportDownload is the presigned URL of a ready export's file (≤ 5
// minutes; 04 §1.14).
type ExportDownload struct {
	URL       string
	ExpiresAt time.Time
}
