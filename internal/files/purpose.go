package files

import (
	"bytes"
	"slices"
)

// Purpose is files.purpose (02 §3.1).
type Purpose string

// Purposes (02 §3.1 files_purpose_chk).
const (
	PurposeAvatar              Purpose = "avatar"
	PurposeCV                  Purpose = "cv"
	PurposeEduDocument         Purpose = "edu_document"
	PurposeOrgLogo             Purpose = "org_logo"
	PurposeEventCover          Purpose = "event_cover"
	PurposePostMedia           Purpose = "post_media"
	PurposeClubClaimEvidence   Purpose = "club_claim_evidence"
	PurposePartyDocument       Purpose = "party_document"
	PurposeBankAccountEvidence Purpose = "bank_account_evidence"
	PurposeTransferReceipt     Purpose = "transfer_receipt"
	PurposePayoutReceipt       Purpose = "payout_receipt"
	PurposeRefundReceipt       Purpose = "refund_receipt"
	PurposeInvoicePDF          Purpose = "invoice_pdf"
	PurposeEventExport         Purpose = "event_export"
	PurposeDSRExport           Purpose = "dsr_export"
	// PurposeDeliveryEvidence is the seller's proof of a delivered order
	// line (orders markOrderLineFulfilled, BE-093, T-292): a photo or a PDF
	// of the seller org, kept with the line as a financial record.
	PurposeDeliveryEvidence Purpose = "delivery_evidence"
)

// MIME types of the allow-lists. SVG, HTML and every other type are never
// accepted (05 §14.7): they can carry script.
const (
	MIMEPDF  = "application/pdf"
	MIMEJPEG = "image/jpeg"
	MIMEPNG  = "image/png"
	MIMEWebP = "image/webp"
	MIMECSV  = "text/csv"
	MIMEXLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	MIMEZIP  = "application/zip"
)

// Size limits (02 §3.1).
const (
	MB = 1 << 20
	// MaxUploadBytes is the largest browser upload of any purpose (05 §14.7,
	// rapor §8.2: 10 MB). Larger limits exist only for server-generated files.
	MaxUploadBytes = 10 * MB
)

// rule is the allow-list row of a purpose (02 §3.1 table).
type rule struct {
	mimes    []string
	maxBytes int64
	// personal: the uploader is the data subject (owner_user_id is set and
	// the file belongs to the user's KVKK export/erasure).
	personal bool
	// org: the purpose belongs to an organisation; createUpload requires
	// org_id ("required"), accepts it ("optional") or rejects it ("").
	org string
	// generated: only PutGenerated writes it (03 k30: never over HTTP).
	generated bool
	// public: a cleared file gets a permanent copy in the public media
	// bucket (BE-120): the images of public records (org logo, event
	// cover). The owning module decides which records show its URL.
	public bool
}

const (
	orgRequired = "required"
	orgOptional = "optional"
)

var (
	images      = []string{MIMEJPEG, MIMEPNG, MIMEWebP}
	documents   = []string{MIMEPDF, MIMEJPEG, MIMEPNG}
	evidence    = []string{MIMEPDF, MIMEJPEG, MIMEPNG, MIMEWebP} // a photo or a PDF
	purposeRule = map[Purpose]rule{
		PurposeAvatar:              {mimes: images, maxBytes: 5 * MB, personal: true},
		PurposeCV:                  {mimes: []string{MIMEPDF}, maxBytes: 10 * MB, personal: true},
		PurposeEduDocument:         {mimes: documents, maxBytes: 10 * MB, personal: true},
		PurposeOrgLogo:             {mimes: images, maxBytes: 10 * MB, org: orgRequired, public: true},
		PurposeEventCover:          {mimes: images, maxBytes: 10 * MB, org: orgRequired, public: true},
		PurposePostMedia:           {mimes: images, maxBytes: 10 * MB, org: orgRequired},
		PurposeClubClaimEvidence:   {mimes: documents, maxBytes: 10 * MB, personal: true},
		PurposePartyDocument:       {mimes: documents, maxBytes: 10 * MB, org: orgOptional},
		PurposeBankAccountEvidence: {mimes: documents, maxBytes: 10 * MB, org: orgOptional},
		PurposeTransferReceipt:     {mimes: documents, maxBytes: 10 * MB},
		PurposePayoutReceipt:       {mimes: documents, maxBytes: 10 * MB},
		PurposeRefundReceipt:       {mimes: documents, maxBytes: 10 * MB},
		PurposeInvoicePDF:          {mimes: documents, maxBytes: 10 * MB, org: orgOptional},
		PurposeEventExport:         {mimes: []string{MIMECSV, MIMEXLSX}, maxBytes: 50 * MB, org: orgOptional, generated: true},
		PurposeDSRExport:           {mimes: []string{MIMEZIP}, maxBytes: 100 * MB, personal: true, generated: true},
		PurposeDeliveryEvidence:    {mimes: evidence, maxBytes: 10 * MB, org: orgRequired},
	}
)

// Valid reports whether p is a purpose of 02 §3.1.
func (p Purpose) Valid() bool { _, ok := purposeRule[p]; return ok }

// Uploadable reports whether browsers may upload p (03 k30: event_export
// and dsr_export are server-generated only).
func (p Purpose) Uploadable() bool { r, ok := purposeRule[p]; return ok && !r.generated }

// MaxBytes is the size limit of p (0 for an unknown purpose).
func (p Purpose) MaxBytes() int64 { return purposeRule[p].maxBytes }

// AllowsMIME reports whether mime is in the allow-list of p.
func (p Purpose) AllowsMIME(mime string) bool { return slices.Contains(purposeRule[p].mimes, mime) }

// Personal reports whether the uploader is the data subject of p.
func (p Purpose) Personal() bool { return purposeRule[p].personal }

// Publishable reports whether a cleared file of p gets a public media copy
// (BE-120: org_logo, event_cover).
func (p Purpose) Publishable() bool { return purposeRule[p].public }

// UploadPurposes lists the purposes browsers may upload, in 02 §3.1 order
// (the OpenAPI enum of createUpload).
func UploadPurposes() []Purpose {
	return []Purpose{
		PurposeAvatar, PurposeCV, PurposeEduDocument, PurposeOrgLogo, PurposeEventCover, PurposePostMedia,
		PurposeClubClaimEvidence, PurposePartyDocument, PurposeBankAccountEvidence, PurposeTransferReceipt,
		PurposePayoutReceipt, PurposeRefundReceipt, PurposeInvoicePDF, PurposeDeliveryEvidence,
	}
}

// UploadMIMEs lists every MIME type a browser upload may declare (the
// OpenAPI enum of createUpload.content_type).
func UploadMIMEs() []string { return []string{MIMEPDF, MIMEJPEG, MIMEPNG, MIMEWebP} }

// SniffLen is how many leading bytes DetectMIME needs.
const SniffLen = 16

// DetectMIME identifies the content by its magic bytes (05 §14.7: the
// declared Content-Type is never trusted). It recognises only the types of
// the browser allow-lists and returns "" for anything else, so an SVG, an
// HTML page or a polyglot that does not start with a known signature is
// rejected.
func DetectMIME(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return MIMEPDF
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return MIMEJPEG
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return MIMEPNG
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return MIMEWebP
	}
	return ""
}

// extension returns the file name extension of a MIME type (download names).
func extension(mime string) string {
	switch mime {
	case MIMEPDF:
		return ".pdf"
	case MIMEJPEG:
		return ".jpg"
	case MIMEPNG:
		return ".png"
	case MIMEWebP:
		return ".webp"
	case MIMECSV:
		return ".csv"
	case MIMEXLSX:
		return ".xlsx"
	case MIMEZIP:
		return ".zip"
	}
	return ""
}
