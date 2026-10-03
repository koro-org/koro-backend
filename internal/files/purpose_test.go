package files_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"koro/internal/files"
)

func TestDetectMIME_MagicBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		head string
		want string
	}{
		{"pdf", "%PDF-1.4\n", files.MIMEPDF},
		{"jpeg", "\xFF\xD8\xFF\xE0\x00\x10JFIF", files.MIMEJPEG},
		{"png", "\x89PNG\r\n\x1a\n\x00\x00", files.MIMEPNG},
		{"webp", "RIFF\x24\x00\x00\x00WEBPVP8 ", files.MIMEWebP},
		{"riff but not webp", "RIFF\x24\x00\x00\x00WAVEfmt ", ""},
		{"svg", `<svg xmlns="http://www.w3.org/2000/svg">`, ""},
		{"svg with xml prolog", `<?xml version="1.0"?><svg>`, ""},
		{"html", "<!DOCTYPE html><html>", ""},
		{"pdf after whitespace", " %PDF-1.4", ""},
		{"zip/docx", "PK\x03\x04\x14\x00", ""},
		{"gif", "GIF89a", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		if got := files.DetectMIME([]byte(tt.head)); got != tt.want {
			t.Errorf("%s: DetectMIME = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestPurpose_AllowListMatchesDesign(t *testing.T) {
	t.Parallel()
	// 02 §3.1: avatar/org images jpeg/png/webp; cv pdf only; documents and
	// receipts pdf/jpeg/png; no SVG, HTML or office formats anywhere.
	for _, p := range files.UploadPurposes() {
		for _, banned := range []string{"image/svg+xml", "text/html", "application/msword", "image/gif"} {
			if p.AllowsMIME(banned) {
				t.Errorf("%s allows %s", p, banned)
			}
		}
		if !p.Uploadable() || !p.Valid() {
			t.Errorf("%s must be uploadable", p)
		}
	}
	if !files.PurposeCV.AllowsMIME(files.MIMEPDF) || files.PurposeCV.AllowsMIME(files.MIMEPNG) {
		t.Error("cv must accept exactly pdf")
	}
	if files.PurposeEventExport.Uploadable() || files.PurposeDSRExport.Uploadable() {
		t.Error("exports are server-generated only (03 k30)")
	}
	if got := files.PurposeAvatar.MaxBytes(); got != 5*files.MB {
		t.Errorf("avatar limit %d", got)
	}
}

func TestDisposition_AlwaysAttachmentWithVerifiedExtension(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, mime, want string
	}{
		{"cv.pdf", files.MIMEPDF, `attachment; filename=cv.pdf`},
		{"rapor.PDF", files.MIMEPDF, `attachment; filename=rapor.PDF`},
		{"photo.jpeg", files.MIMEJPEG, `attachment; filename=photo.jpeg`},
		{"evil.html", files.MIMEPDF, `attachment; filename=evil.pdf`},
		{"", files.MIMEPNG, `attachment; filename=file.png`},
		{`a"b.pdf`, files.MIMEPDF, `attachment; filename="a\"b.pdf"`},
		{"Özgeçmiş.pdf", files.MIMEPDF, `attachment; filename*=utf-8''%C3%96zge%C3%A7mi%C5%9F.pdf`},
	}
	for _, tt := range tests {
		if got := files.Disposition(tt.name, tt.mime); got != tt.want {
			t.Errorf("Disposition(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRegister_EnumsMatchAllowList(t *testing.T) {
	t.Parallel()
	_, api := humatest.New(t)
	files.Register(api, nil)
	o := api.OpenAPI().Paths["/files/uploads"].Post
	if o == nil || o.OperationID != "createUpload" {
		t.Fatal("createUpload not registered")
	}
	schema := api.OpenAPI().Components.Schemas.SchemaFromRef(o.RequestBody.Content["application/json"].Schema.Ref)
	enum := func(prop string) []string {
		var out []string
		for _, v := range schema.Properties[prop].Enum {
			out = append(out, v.(string))
		}
		return out
	}
	var purposes []string
	for _, p := range files.UploadPurposes() {
		purposes = append(purposes, string(p))
	}
	if got := enum("purpose"); !slices.Equal(got, purposes) {
		t.Errorf("purpose enum %v, want %v", got, purposes)
	}
	if got := enum("content_type"); !slices.Equal(got, files.UploadMIMEs()) {
		t.Errorf("content_type enum %v, want %v", got, files.UploadMIMEs())
	}
	for _, m := range files.UploadMIMEs() {
		if !slices.ContainsFunc(files.UploadPurposes(), func(p files.Purpose) bool { return p.AllowsMIME(m) }) {
			t.Errorf("%s is in the enum but no purpose allows it", m)
		}
	}
	for _, path := range []string{"/files/{file_id}/complete", "/files/{file_id}"} {
		if api.OpenAPI().Paths[path] == nil {
			t.Errorf("%s not registered", path)
		}
	}
	if !strings.Contains(o.Description, "presigned") {
		t.Error("createUpload lacks its description")
	}
}
