package domainevent

import "uuid"

// Topics of the files module (01 §6.2). Owner: files tickets.
const (
	TopicFileUploaded  = "file.uploaded"
	TopicFileRejected  = "file.rejected"
	TopicFilePublished = "file.published"
)

var _ = register(
	spec("files", AfterCommit, FileUploaded{}),
	spec("files", Outbox, FileRejected{}),
	spec("files", Outbox, FilePublished{}),
)

// FileUploaded: the ClamAV scan is triggered.
type FileUploaded struct {
	FileID uuid.UUID `json:"file_id"`
}

// Topic implements Payload.
func (FileUploaded) Topic() string { return TopicFileUploaded }

// FileRejected: a file was taken out of use after its upload (BE-048):
// malware was found (Reason "infected") or its content could not be
// processed (Reason "unprocessable", BE-059). The object is already
// deleted. files notifies the uploader; an owning module that attached the
// file (AttachedModule, AttachedEntityID) drops its reference and deletes
// the file. Purpose is the files.purpose code.
type FileRejected struct {
	FileID           uuid.UUID  `json:"file_id"`
	UploaderID       *uuid.UUID `json:"uploader_id"`
	Purpose          string     `json:"purpose"`
	Reason           string     `json:"reason"`
	AttachedModule   string     `json:"attached_module"`
	AttachedEntityID *uuid.UUID `json:"attached_entity_id"`
}

// Topic implements Payload.
func (FileRejected) Topic() string { return TopicFileRejected }

// FilePublished: a publishable file (Purpose org_logo or event_cover)
// attached to its owner (AttachedModule, AttachedEntityID: the org or the
// event) got its public media copy (BE-120). Written in the transaction
// that records the copy, only for an attached file: the owner's public
// page may now show the image, so the web revalidates it (webcache). A
// file attached after its copy needs no event: the owner's own update
// event (org.updated, event.updated) already revalidates the page.
type FilePublished struct {
	FileID           uuid.UUID `json:"file_id"`
	Purpose          string    `json:"purpose"`
	AttachedModule   string    `json:"attached_module"`
	AttachedEntityID uuid.UUID `json:"attached_entity_id"`
}

// Topic implements Payload.
func (FilePublished) Topic() string { return TopicFilePublished }
