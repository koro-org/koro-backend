package notify_test

import (
	htmltemplate "html/template"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"koro/internal/notify"
)

// baseTemplates copies templates/ from disk into a MapFS the tests can extend
// (the embedded FS is not reachable from this black-box package).
func baseTemplates(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	root := os.DirFS("templates")
	err := fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(root, p)
		m[p] = &fstest.MapFile{Data: b}
		return err
	})
	if err != nil {
		t.Fatalf("read templates/: %v", err)
	}
	return m
}

// withPromo adds a commercial sample template.
func withPromo(m fstest.MapFS) fstest.MapFS {
	files := map[string]string{
		"notify/test_promo.meta.yaml":       "version: 3\ncategory: commercial\n",
		"notify/test_promo.tr.subject.tmpl": "Kampanya: {{.Data.name}}",
		"notify/test_promo.en.subject.tmpl": "Offer: {{.Data.name}}",
		"notify/test_promo.tr.txt.tmpl":     "Merhaba {{.Data.name}}, yeni teklifler var.",
		"notify/test_promo.en.txt.tmpl":     "Hello {{.Data.name}}, there are new offers.",
		"notify/test_promo.tr.html.tmpl":    "<p>Merhaba {{.Data.name}}, yeni teklifler var.</p>",
		"notify/test_promo.en.html.tmpl":    "<p>Hello {{.Data.name}}, there are new offers.</p>",
	}
	for p, s := range files {
		m[p] = &fstest.MapFile{Data: []byte(s)}
	}
	return m
}

func loadTemplates(t *testing.T, fsys fs.FS) *notify.Templates {
	t.Helper()
	tpl, err := notify.LoadTemplates(fsys)
	if err != nil {
		t.Fatalf("LoadTemplates() error = %v", err)
	}
	return tpl
}

func TestTemplates_AllParse(t *testing.T) {
	t.Parallel()
	tpl, err := notify.EmbeddedTemplates()
	if err != nil {
		t.Fatalf("EmbeddedTemplates() error = %v", err)
	}
	ids := tpl.IDs()
	if len(ids) == 0 {
		t.Fatal("no templates are embedded")
	}
	found := false
	for _, id := range ids {
		version, category, ok := tpl.Info(id)
		if !ok || version < 1 || (category != notify.CategoryTransactional && category != notify.CategoryCommercial) {
			t.Errorf("Info(%q) = (%d, %q, %v), want a positive version and a valid category", id, version, category, ok)
		}
		// Every template renders in both locales; nil Data only fails on missing keys,
		// so give it the sample keys.
		for _, loc := range []string{"tr", "en"} {
			r, err := tpl.Render(id, loc, notify.RenderInput{Data: map[string]any{"name": "Test"}, UnsubscribeURL: "https://koro.test/u"})
			if err != nil {
				t.Errorf("Render(%q, %s) error = %v", id, loc, err)
				continue
			}
			if r.Locale != loc || r.Subject == "" || r.Text == "" || !strings.Contains(r.HTML, "<html") {
				t.Errorf("Render(%q, %s) = %+v, want a locale-matching subject, text and HTML document", id, loc, r)
			}
		}
		found = found || id == "notify.test_message"
	}
	if !found {
		t.Fatalf("notify.test_message is not among %v", ids)
	}
}

func TestTemplates_LoadErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(m fstest.MapFS)
		wantErr string
	}{
		{"missing meta", func(m fstest.MapFS) { delete(m, "notify/test_message.meta.yaml") }, "meta.yaml is missing"},
		{"bad category", func(m fstest.MapFS) {
			m["notify/test_message.meta.yaml"] = &fstest.MapFile{Data: []byte("version: 1\ncategory: marketing\n")}
		}, "category must be"},
		{"zero version", func(m fstest.MapFS) {
			m["notify/test_message.meta.yaml"] = &fstest.MapFile{Data: []byte("version: 0\ncategory: transactional\n")}
		}, "version must be"},
		{"unknown meta field", func(m fstest.MapFS) {
			m["notify/test_message.meta.yaml"] = &fstest.MapFile{Data: []byte("version: 1\ncategory: transactional\nowner: me\n")}
		}, "owner"},
		{"missing english html", func(m fstest.MapFS) { delete(m, "notify/test_message.en.html.tmpl") }, "en.html.tmpl is missing"},
		{"missing turkish subject", func(m fstest.MapFS) { delete(m, "notify/test_message.tr.subject.tmpl") }, "tr.subject.tmpl is missing"},
		{"unsupported locale", func(m fstest.MapFS) {
			m["notify/test_message.fr.txt.tmpl"] = &fstest.MapFile{Data: []byte("x")}
		}, `unsupported locale "fr"`},
		{"stray file", func(m fstest.MapFS) { m["notify/README.md"] = &fstest.MapFile{Data: []byte("x")} }, "unexpected file"},
		{"file outside a module directory", func(m fstest.MapFS) { m["top.tmpl"] = &fstest.MapFile{Data: []byte("x")} }, "templates live in"},
		{"nested directory", func(m fstest.MapFS) {
			m["notify/deep/x.tr.txt.tmpl"] = &fstest.MapFile{Data: []byte("x")}
		}, "templates live in"},
		{"missing layout", func(m fstest.MapFS) { delete(m, "layout/base.en.txt.tmpl") }, "layout/base.en.txt.tmpl is missing"},
		{"syntax error", func(m fstest.MapFS) {
			m["notify/test_message.tr.txt.tmpl"] = &fstest.MapFile{Data: []byte("Merhaba {{.Data.name")}
		}, "tr txt"},
		{"unknown function", func(m fstest.MapFS) {
			m["notify/test_message.tr.subject.tmpl"] = &fstest.MapFile{Data: []byte("{{nope .Data.name}}")}
		}, "tr subject"},
		{"data: URI image", func(m fstest.MapFS) {
			m["notify/test_message.tr.html.tmpl"] = &fstest.MapFile{Data: []byte(`<img src="data:image/png;base64,AAAA">`)}
		}, "data: URIs are not allowed"},
		{"data: URI in CSS", func(m fstest.MapFS) {
			m["notify/test_message.en.html.tmpl"] = &fstest.MapFile{Data: []byte(`<div style="background:url( 'data:image/png;base64,AAAA')">x</div>`)}
		}, "data: URIs are not allowed"},
		{"html escaping error found at load, not at first send", func(m fstest.MapFS) {
			m["notify/test_message.en.html.tmpl"] = &fstest.MapFile{Data: []byte(`<a href="{{.Data.name}}`)}
		}, "en html"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := baseTemplates(t)
			tt.mutate(m)
			_, err := notify.LoadTemplates(m)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadTemplates() error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
	if _, err := notify.LoadTemplates(fstest.MapFS{}); err == nil {
		t.Fatal("LoadTemplates(empty) succeeded")
	}
}

func TestTemplates_HTMLEscapesData(t *testing.T) {
	t.Parallel()
	tpl := loadTemplates(t, baseTemplates(t))

	const payload = `<script>alert("x")</script><img src=x onerror=alert(1)>`
	r, err := tpl.Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": payload}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, raw := range []string{"<script>", "</script>", "<img src=x", `alert("x")`} {
		if strings.Contains(r.HTML, raw) {
			t.Fatalf("HTML carries unescaped %q:\n%s", raw, r.HTML)
		}
	}
	if !strings.Contains(r.HTML, "&lt;script&gt;") {
		t.Fatalf("HTML does not carry the escaped payload:\n%s", r.HTML)
	}
	// The plain-text alternative is not HTML and shows the text as written.
	if !strings.Contains(r.Text, payload) {
		t.Fatalf("text part = %q, want the data verbatim", r.Text)
	}
	if strings.Contains(r.Subject, "\n") {
		t.Fatal("subject spans lines")
	}
}

func TestTemplates_RefusesPreEscapedData(t *testing.T) {
	t.Parallel()
	tpl := loadTemplates(t, baseTemplates(t))
	type nested struct{ Link htmltemplate.URL }
	tests := map[string]any{
		"html":        htmltemplate.HTML("<b>x</b>"),
		"js":          htmltemplate.JS("alert(1)"),
		"url":         htmltemplate.URL("javascript:alert(1)"),
		"in a slice":  []any{"ok", htmltemplate.HTMLAttr(`onclick="x"`)},
		"in a map":    map[string]any{"deep": map[string]any{"x": htmltemplate.CSS("a{}")}},
		"in a struct": nested{Link: "x"},
		"by pointer":  &nested{},
	}
	for name, v := range tests {
		_, err := tpl.Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": v}})
		if err == nil || !strings.Contains(err.Error(), "pre-escaped") && !fieldsMention(err, "pre-escaped") {
			t.Errorf("%s: Render() error = %v, want a pre-escaped type refusal", name, err)
		}
	}
	if _, err := tpl.Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": "plain", "list": []string{"a"}, "n": 3}}); err != nil {
		t.Fatalf("plain data refused: %v", err)
	}
}

func fieldsMention(err error, sub string) bool {
	for _, f := range fieldsOf(err) {
		if strings.Contains(f.Message, sub) {
			return true
		}
	}
	return false
}

func TestTemplates_CIDReferenceIsNotRewritten(t *testing.T) {
	t.Parallel()
	tpl := loadTemplates(t, baseTemplates(t))
	r, err := tpl.Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": "A", "qr_cid": "qr.png"}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(r.HTML, `src="cid:qr.png"`) {
		t.Fatalf("html/template rewrote the cid: URL:\n%s", r.HTML)
	}
}

func TestTemplates_MissingDataKeyFails(t *testing.T) {
	t.Parallel()
	tpl := loadTemplates(t, baseTemplates(t))
	for _, data := range []map[string]any{nil, {}, {"nome": "typo"}} {
		if _, err := tpl.Render("notify.test_message", "tr", notify.RenderInput{Data: data}); err == nil {
			t.Errorf("Render with data %v succeeded: a mail must never carry an empty placeholder", data)
		}
	}
}

func TestTemplates_UnknownTemplate(t *testing.T) {
	t.Parallel()
	tpl := loadTemplates(t, baseTemplates(t))
	if _, err := tpl.Render("notify.nope", "tr", notify.RenderInput{}); err == nil {
		t.Fatal("Render(unknown) succeeded")
	}
	if _, _, ok := tpl.Info("notify.nope"); ok {
		t.Fatal("Info(unknown) reported the template")
	}
}

func TestTemplates_LocaleResolution(t *testing.T) {
	t.Parallel()
	tpl := loadTemplates(t, baseTemplates(t))
	tests := []struct{ in, want, subject string }{
		{"", "tr", "Koro test iletisi"},
		{"tr", "tr", "Koro test iletisi"},
		{"tr-TR", "tr", "Koro test iletisi"},
		{"en", "en", "Koro test message"},
		{"EN_us", "en", "Koro test message"},
		{" en-GB ", "en", "Koro test message"},
		{"fr", "tr", "Koro test iletisi"},
		{"zz-ZZ", "tr", "Koro test iletisi"},
	}
	for _, tt := range tests {
		r, err := tpl.Render("notify.test_message", tt.in, notify.RenderInput{Data: map[string]any{"name": "A"}})
		if err != nil || r.Locale != tt.want || r.Subject != tt.subject {
			t.Errorf("Render(locale %q) = (%q, %q, %v), want (%q, %q)", tt.in, r.Locale, r.Subject, err, tt.want, tt.subject)
		}
	}
}

func TestTemplates_SubjectIsAlwaysOneLine(t *testing.T) {
	t.Parallel()
	m := baseTemplates(t)
	m["notify/test_message.tr.subject.tmpl"] = &fstest.MapFile{Data: []byte("Merhaba {{.Data.name}}\n")}
	tpl := loadTemplates(t, m)

	r, err := tpl.Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": "Ayşe\r\nBcc: kurban@example.com"}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if strings.ContainsAny(r.Subject, "\r\n") || r.Subject != "Merhaba Ayşe Bcc: kurban@example.com" {
		t.Fatalf("subject = %q, want one line", r.Subject)
	}

	m["notify/test_message.tr.subject.tmpl"] = &fstest.MapFile{Data: []byte("   {{\"\"}}  ")}
	if _, err := loadTemplates(t, m).Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": "A"}}); err == nil {
		t.Fatal("an empty subject was accepted")
	}
	m["notify/test_message.tr.subject.tmpl"] = &fstest.MapFile{Data: []byte(strings.Repeat("x", 300))}
	if _, err := loadTemplates(t, m).Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": "A"}}); err == nil {
		t.Fatal("a 300-character subject was accepted")
	}
}

// The footer is chosen by the template's declared category, so an informational
// mail cannot carry a promotional footer and a commercial mail cannot lose its
// opt-out (Law 6563: the two kinds must stay apart).
func TestTemplates_FooterFollowsCategory(t *testing.T) {
	t.Parallel()
	tpl := loadTemplates(t, withPromo(baseTemplates(t)))
	in := notify.RenderInput{Data: map[string]any{"name": "A"}, UnsubscribeURL: "https://koro.test/u/abc"}

	info, err := tpl.Render("notify.test_message", "tr", in)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{info.Text, info.HTML} {
		if !strings.Contains(body, "ticari elektronik ileti değildir") || strings.Contains(body, "koro.test/u/abc") {
			t.Fatalf("transactional footer wrong:\n%s", body)
		}
	}

	for _, loc := range []string{"tr", "en"} {
		promo, err := tpl.Render("notify.test_promo", loc, in)
		if err != nil {
			t.Fatal(err)
		}
		if promo.Version != 3 || promo.Category != notify.CategoryCommercial {
			t.Fatalf("promo = version %d category %s, want 3 commercial", promo.Version, promo.Category)
		}
		for _, body := range []string{promo.Text, promo.HTML} {
			if !strings.Contains(body, "https://koro.test/u/abc") || strings.Contains(body, "değildir") || strings.Contains(body, "is not a commercial") {
				t.Fatalf("commercial footer (%s) wrong:\n%s", loc, body)
			}
		}
	}
}

func TestTemplates_DateFunctionsUseIstanbul(t *testing.T) {
	t.Parallel()
	m := baseTemplates(t)
	m["notify/test_message.tr.subject.tmpl"] = &fstest.MapFile{Data: []byte("{{datetime .Data.at}} / {{date .Data.at}}")}
	tpl := loadTemplates(t, m)

	at := time.Date(2026, 12, 31, 22, 30, 0, 0, time.UTC) // 01:30 on 1 Jan in Istanbul (UTC+3)
	r, err := tpl.Render("notify.test_message", "tr", notify.RenderInput{Data: map[string]any{"name": "A", "at": at}})
	if err != nil || r.Subject != "01.01.2027 01:30 / 01.01.2027" {
		t.Fatalf("subject = %q, %v, want Istanbul local time", r.Subject, err)
	}
}
