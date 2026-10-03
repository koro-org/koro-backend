package domainevent

import "uuid"

// Topics of the files module (01 §6.2). Owner: files tickets.
const (
	TopicFileUploaded = "file.uploaded"
)

var _ = register(
	spec("files", AfterCommit, FileUploaded{}),
)

// FileUploaded: the ClamAV scan is triggered.
type FileUploaded struct {
	FileID uuid.UUID `json:"file_id"`
}

// Topic implements Payload.
func (FileUploaded) Topic() string { return TopicFileUploaded }
