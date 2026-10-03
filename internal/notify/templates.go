package notify

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"io"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	texttemplate "text/template"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"koro/internal/platform/clock"
	"koro/internal/platform/problem"
)

// Template files (BE-005 ticket, R8; BE-015). The directory layout is
//
//	templates/<module>/<name>.meta.yaml                    version, category, preference, channels
//	templates/<module>/<name>.<locale>.subject.tmpl        e-mail subject (one line, plain text)
//	templates/<module>/<name>.<locale>.txt.tmpl            e-mail plain-text body
//	templates/<module>/<name>.<locale>.html.tmpl           e-mail HTML body (html/template, auto-escaped)
//	templates/<module>/<name>.<locale>.title.tmpl          in_app/push title (one line, ≤ 140 runes)
//	templates/<module>/<name>.<locale>.body.tmpl           in_app/push body (≤ 500 runes)
//	templates/layout/base.<locale>.{txt,html}.tmpl         shared wrapper and category footer
//
// The meta file:
//
//	version: 1                    # → notification_deliveries.template_version
//	category: transactional       # transactional | commercial
//	preference: event_updates     # transactional only: the preference category (02 §6.5)
//	channels: [email, in_app]     # default [email]; email needs subject/txt/html,
//	                              # in_app and push need title/body
//
// in_app and push texts are fixed: they are rendered once at load time with
// {{.AppName}} and {{.Locale}} only. Data never reaches them, so no personal
// data can appear in an in-app notification or a push payload ("Yeni bir
// mesajınız var" + an opaque ID in the deep link or the data).
//
// A module adds only new files under its own directory (BE-001 R8); the
// directive below picks them up without touching this package. The template
// ID in a Message is "<module>.<name>".
//
//go:embed templates
var embeddedFS embed.FS

// DefaultLocale is used when the requested locale is empty or unsupported.
const DefaultLocale = "tr"

// supportedLocales are the locales every template must provide.
var supportedLocales = []string{"tr", "en"}

const (
	layoutDir   = "layout"
	layoutName  = "base"
	appName     = "Koro"
	maxSubject  = 255 // runes; longer subjects are a template bug
	maxSmallInt = 1<<15 - 1
)

var (
	// R-003 §2.5.6: data: images are unreliable in mail clients (Gmail, Outlook);
	// inline images go out as CID parts.
	reDataURI = regexp.MustCompile(`(?i)(?:\b(?:src|srcset|href|background)\s*=\s*["']?\s*|url\(\s*["']?\s*)data:`)

	reIdentPart   = regexp.MustCompile(`^[a-z_]+$`)
	reTemplateID  = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)*$`) // 02 §6.1 CHECK
	templateParts = []string{"subject", "txt", "html"}
	noticeParts   = []string{"title", "body"}
)

// Limits of the fixed in_app/push texts (in_app_notifications CHECKs).
const (
	maxNoticeTitle = 140
	maxNoticeBody  = 500
)

// Templates is the set of parsed, versioned message templates. It is
// immutable after loading and safe for concurrent use.
type Templates struct {
	sets map[string]*templateSet
}

type templateSet struct {
	version    int
	category   Category
	preference PrefCategory // "" for commercial templates
	channels   []Channel
	locales    map[string]*localized
	notices    map[string]Notice // locale → fixed in_app/push text
}

// Notice is the fixed visible text of an in_app or push message.
type Notice struct {
	Title string
	Body  string
}

// TemplateMeta is what a template's meta file declares.
type TemplateMeta struct {
	Version    int
	Category   Category
	Preference PrefCategory // "" for commercial templates
	Channels   []Channel
}

// Supports reports whether the template serves channel ch.
func (m TemplateMeta) Supports(ch Channel) bool { return slices.Contains(m.Channels, ch) }

type localized struct {
	subject *texttemplate.Template
	text    *texttemplate.Template
	html    *htmltemplate.Template
}

// RenderInput is what Render needs besides the template ID and locale.
type RenderInput struct {
	Data           map[string]any
	UnsubscribeURL string // shown in the footer of commercial templates
}

// Rendered is a fully rendered e-mail body.
type Rendered struct {
	Subject  string // single line
	Text     string
	HTML     string
	Version  int
	Category Category
	Locale   string // the locale actually used
}

// templateData is the value templates are executed with: {{.Data.key}},
// {{.Locale}}, {{.Category}}, {{.UnsubscribeURL}}, {{.AppName}}.
type templateData struct {
	Data           map[string]any
	Locale         string
	Category       string
	UnsubscribeURL string
	AppName        string
}

// EmbeddedTemplates parses the templates compiled into the binary. A missing
// or broken template fails here, at start (TestTemplates_AllParse).
func EmbeddedTemplates() (*Templates, error) {
	sub, err := fs.Sub(embeddedFS, "templates")
	if err != nil {
		return nil, fmt.Errorf("notify: embedded templates: %w", err)
	}
	return LoadTemplates(sub)
}

// LoadTemplates parses every template under fsys (laid out as documented
// above) and reports all problems at once: unknown files, a missing locale or
// part, a missing or invalid meta file, template syntax and HTML escaping
// errors.
func LoadTemplates(fsys fs.FS) (*Templates, error) {
	files, err := scanTemplateFiles(fsys)
	if err != nil {
		return nil, err
	}
	var errs []error
	errs = append(errs, files.errs...)

	layouts := map[string]map[string]string{} // part → locale → source
	for _, part := range []string{"txt", "html"} {
		layouts[part] = map[string]string{}
		for _, loc := range supportedLocales {
			src, ok := files.layout[layoutKey{loc, part}]
			if !ok {
				errs = append(errs, fmt.Errorf("layout/%s.%s.%s.tmpl is missing", layoutName, loc, part))
				continue
			}
			layouts[part][loc] = src
		}
	}

	t := &Templates{sets: map[string]*templateSet{}}
	for _, id := range sortedKeys(files.sets) {
		raw := files.sets[id]
		set, err := raw.build(id, layouts)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		t.sets[id] = set
	}
	if len(t.sets) == 0 && len(errs) == 0 {
		errs = append(errs, errors.New("no templates found"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("notify: templates: %w", err)
	}
	return t, nil
}

// IDs returns the template IDs, sorted.
func (t *Templates) IDs() []string { return sortedKeys(t.sets) }

// Info returns the version and category declared by the template's meta file.
func (t *Templates) Info(id string) (version int, category Category, ok bool) {
	set, ok := t.sets[id]
	if !ok {
		return 0, "", false
	}
	return set.version, set.category, true
}

// Meta returns everything the template's meta file declares.
func (t *Templates) Meta(id string) (TemplateMeta, bool) {
	set, ok := t.sets[id]
	if !ok {
		return TemplateMeta{}, false
	}
	return TemplateMeta{Version: set.version, Category: set.category, Preference: set.preference,
		Channels: slices.Clone(set.channels)}, true
}

// Notice returns the fixed in_app/push text of template id in the requested
// locale (falling back to DefaultLocale); ok=false when the template does
// not serve in_app or push.
func (t *Templates) Notice(id, locale string) (Notice, bool) {
	set, ok := t.sets[id]
	if !ok || set.notices == nil {
		return Notice{}, false
	}
	n, ok := set.notices[resolveLocale(locale)]
	return n, ok
}

// Render renders template id in the requested locale (falling back to
// DefaultLocale). A template that reads a Data key the caller did not supply
// fails instead of printing an empty value.
func (t *Templates) Render(id, locale string, in RenderInput) (Rendered, error) {
	set, ok := t.sets[id]
	if !ok {
		return Rendered{}, ErrInvalidMessage.WithFields(problem.FieldError{Location: "message.template", Message: "unknown template"})
	}
	if !slices.Contains(set.channels, ChannelEmail) {
		return Rendered{}, ErrInvalidMessage.WithFields(problem.FieldError{Location: "message.channel", Message: "the template does not serve this channel"})
	}
	// html/template trusts these types and would emit them unescaped; a caller
	// that wraps user input in them (05 §14.6 forbids it) is refused here.
	if p := findPreEscaped(reflect.ValueOf(in.Data), "data", 0); p != "" {
		return Rendered{}, ErrInvalidMessage.WithFields(problem.FieldError{
			Location: "message.data", Message: p + " is a pre-escaped template type (template.HTML, JS, URL, ...); pass plain strings",
		})
	}
	loc := resolveLocale(locale)
	l := set.locales[loc]
	data := templateData{
		Data: in.Data, Locale: loc, Category: string(set.category),
		UnsubscribeURL: in.UnsubscribeURL, AppName: appName,
	}

	var subject, text, html bytes.Buffer
	for _, step := range []struct {
		part string
		exec func() error
	}{
		{"subject", func() error { return l.subject.Execute(&subject, data) }},
		{"txt", func() error { return l.text.Execute(&text, data) }},
		{"html", func() error { return l.html.Execute(&html, data) }},
	} {
		if err := step.exec(); err != nil {
			return Rendered{}, ErrInvalidMessage.
				WithFields(problem.FieldError{Location: "message.data", Message: "template cannot be rendered with this data (" + step.part + ")"}).
				Wrap(err)
		}
	}

	line := strings.Join(strings.Fields(subject.String()), " ") // one line: no header injection
	switch {
	case line == "":
		return Rendered{}, ErrInvalidMessage.WithFields(problem.FieldError{Location: "message.template", Message: "rendered subject is empty"})
	case utf8.RuneCountInString(line) > maxSubject:
		return Rendered{}, ErrInvalidMessage.WithFields(problem.FieldError{Location: "message.template", Message: "rendered subject is too long"})
	}
	return Rendered{
		Subject: line, Text: text.String(), HTML: html.String(),
		Version: set.version, Category: set.category, Locale: loc,
	}, nil
}

var preEscapedTypes = []reflect.Type{
	reflect.TypeFor[htmltemplate.HTML](), reflect.TypeFor[htmltemplate.HTMLAttr](), reflect.TypeFor[htmltemplate.JS](),
	reflect.TypeFor[htmltemplate.JSStr](), reflect.TypeFor[htmltemplate.CSS](), reflect.TypeFor[htmltemplate.URL](),
	reflect.TypeFor[htmltemplate.Srcset](),
}

// findPreEscaped returns the path of the first html/template "trusted" value
// inside v, or "". It walks maps, slices, arrays, pointers and exported
// struct fields up to a small depth.
func findPreEscaped(v reflect.Value, path string, depth int) string {
	if !v.IsValid() || depth > 8 {
		return ""
	}
	if slices.Contains(preEscapedTypes, v.Type()) {
		return path
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !v.IsNil() {
			return findPreEscaped(v.Elem(), path, depth+1)
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			if p := findPreEscaped(v.MapIndex(k), path+"."+fmt.Sprint(k), depth+1); p != "" {
				return p
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			if p := findPreEscaped(v.Index(i), fmt.Sprintf("%s[%d]", path, i), depth+1); p != "" {
				return p
			}
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if f := v.Type().Field(i); f.IsExported() {
				if p := findPreEscaped(v.Field(i), path+"."+f.Name, depth+1); p != "" {
					return p
				}
			}
		}
	default:
	}
	return ""
}

// resolveLocale maps "tr-TR", "EN_us", "" ... to a supported locale.
func resolveLocale(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if i := strings.IndexAny(l, "-_"); i >= 0 {
		l = l[:i]
	}
	if slices.Contains(supportedLocales, l) {
		return l
	}
	return DefaultLocale
}

// --- file scanning ---------------------------------------------------------

type layoutKey struct{ locale, part string }

type scannedFiles struct {
	layout map[layoutKey]string
	sets   map[string]*rawSet
	errs   []error
}

type rawSet struct {
	meta  []byte
	parts map[layoutKey]string // locale + part → source
}

func scanTemplateFiles(fsys fs.FS) (*scannedFiles, error) {
	out := &scannedFiles{layout: map[layoutKey]string{}, sets: map[string]*rawSet{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		src, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		out.add(p, string(src))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("notify: scan templates: %w", err)
	}
	return out, nil
}

func (s *scannedFiles) add(p, src string) {
	dir, file := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" || strings.Contains(dir, "/") || !reIdentPart.MatchString(dir) {
		s.errs = append(s.errs, fmt.Errorf("%s: templates live in templates/<module>/<file>", p))
		return
	}
	fields := strings.Split(file, ".")

	// <name>.meta.yaml
	if len(fields) == 3 && fields[1] == "meta" && fields[2] == "yaml" && dir != layoutDir {
		if !reIdentPart.MatchString(fields[0]) {
			s.errs = append(s.errs, fmt.Errorf("%s: template name must match [a-z_]+", p))
			return
		}
		s.set(dir + "." + fields[0]).meta = []byte(src)
		return
	}
	// <name>.<locale>.<part>.tmpl
	if len(fields) != 4 || fields[3] != "tmpl" || !reIdentPart.MatchString(fields[0]) {
		s.errs = append(s.errs, fmt.Errorf("%s: unexpected file (want <name>.<locale>.<subject|txt|html|title|body>.tmpl or <name>.meta.yaml)", p))
		return
	}
	name, locale, part := fields[0], fields[1], fields[2]
	if !slices.Contains(supportedLocales, locale) {
		s.errs = append(s.errs, fmt.Errorf("%s: unsupported locale %q (supported: %s)", p, locale, strings.Join(supportedLocales, ", ")))
		return
	}
	if dir == layoutDir {
		if name != layoutName || (part != "txt" && part != "html") {
			s.errs = append(s.errs, fmt.Errorf("%s: layout files are %s.<locale>.<txt|html>.tmpl", p, layoutName))
			return
		}
		s.layout[layoutKey{locale, part}] = src
		return
	}
	if !slices.Contains(templateParts, part) && !slices.Contains(noticeParts, part) {
		s.errs = append(s.errs, fmt.Errorf("%s: unknown part %q (want subject, txt, html, title or body)", p, part))
		return
	}
	s.set(dir + "." + name).parts[layoutKey{locale, part}] = src
}

func (s *scannedFiles) set(id string) *rawSet {
	r, ok := s.sets[id]
	if !ok {
		r = &rawSet{parts: map[layoutKey]string{}}
		s.sets[id] = r
	}
	return r
}

// --- building --------------------------------------------------------------

type metaFile struct {
	Version    int      `yaml:"version"`
	Category   string   `yaml:"category"`
	Preference string   `yaml:"preference"`
	Channels   []string `yaml:"channels"`
}

func (r *rawSet) build(id string, layouts map[string]map[string]string) (*templateSet, error) {
	var errs []error
	fail := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("%s: "+format, append([]any{id}, a...)...))
	}

	set := &templateSet{locales: map[string]*localized{}, channels: []Channel{ChannelEmail}}
	if r.meta == nil {
		fail("%s.meta.yaml is missing", strings.SplitN(id, ".", 2)[1])
	} else {
		var m metaFile
		dec := yaml.NewDecoder(bytes.NewReader(r.meta))
		dec.KnownFields(true)
		if err := dec.Decode(&m); err != nil {
			fail("meta file: %v", err)
		}
		if m.Version < 1 || m.Version > maxSmallInt {
			fail("meta version must be between 1 and %d", maxSmallInt)
		}
		set.version = m.Version
		set.category = Category(m.Category)
		if !set.category.valid() {
			fail("meta category must be transactional or commercial")
		}
		set.preference = PrefCategory(m.Preference)
		switch {
		case set.category == CategoryCommercial && m.Preference != "":
			fail("meta preference is for transactional templates only (commercial messages are governed by consent)")
		case set.category == CategoryTransactional && !set.preference.valid():
			fail("meta preference must be one of %v (02 §6.5)", PrefCategories())
		}
		if m.Channels != nil {
			set.channels = nil
			for _, c := range m.Channels {
				ch := Channel(c)
				switch {
				case !ch.valid():
					fail("meta channel %q must be email, push or in_app", c)
				case slices.Contains(set.channels, ch):
					fail("meta channel %q is listed twice", c)
				default:
					set.channels = append(set.channels, ch)
				}
			}
			if len(set.channels) == 0 {
				fail("meta channels must not be empty")
			}
		}
		if set.category == CategoryCommercial && slices.Contains(set.channels, ChannelInApp) {
			fail("commercial templates cannot serve in_app")
		}
	}
	email := slices.Contains(set.channels, ChannelEmail)
	notice := slices.Contains(set.channels, ChannelInApp) || slices.Contains(set.channels, ChannelPush)
	for k := range r.parts {
		switch {
		case slices.Contains(templateParts, k.part) && !email:
			fail("%s.%s.tmpl is an e-mail part but the template does not list the email channel", k.locale, k.part)
		case slices.Contains(noticeParts, k.part) && !notice:
			fail("%s.%s.tmpl is an in_app/push part but the template lists neither in_app nor push", k.locale, k.part)
		}
	}
	if notice {
		set.notices = map[string]Notice{}
		for _, loc := range supportedLocales {
			n, err := buildNotice(id, loc, r.parts)
			if err != nil {
				fail("%v", err)
				continue
			}
			set.notices[loc] = n
		}
	}
	if !email {
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
		return set, nil
	}

	for _, loc := range supportedLocales {
		l := &localized{}
		complete := true
		for _, part := range templateParts {
			if _, ok := r.parts[layoutKey{loc, part}]; !ok {
				fail("%s.%s.tmpl is missing", loc, part)
				complete = false
			}
		}
		if !complete {
			continue
		}
		var err error
		if l.subject, err = parseText(id+"."+loc+".subject", "", r.parts[layoutKey{loc, "subject"}]); err != nil {
			fail("%s subject: %v", loc, err)
		}
		if l.text, err = parseText(id+"."+loc+".txt", layouts["txt"][loc], r.parts[layoutKey{loc, "txt"}]); err != nil {
			fail("%s txt: %v", loc, err)
		}
		if l.html, err = parseHTML(id+"."+loc+".html", layouts["html"][loc], r.parts[layoutKey{loc, "html"}]); err != nil {
			fail("%s html: %v", loc, err)
		}
		set.locales[loc] = l
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return set, nil
}

var textFuncs = texttemplate.FuncMap{"datetime": formatDateTime, "date": formatDate}

var htmlFuncs = htmltemplate.FuncMap{"datetime": formatDateTime, "date": formatDate}

// parseText parses a text/template. With a layout, the executed template is
// the layout and the body is its "content".
func parseText(name, layout, body string) (*texttemplate.Template, error) {
	if layout == "" {
		return texttemplate.New(name).Funcs(textFuncs).Option("missingkey=error").Parse(body)
	}
	t := texttemplate.New(layoutName).Funcs(textFuncs).Option("missingkey=error")
	if _, err := t.Parse(layout); err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	if _, err := t.New("content").Parse(body); err != nil {
		return nil, err
	}
	return t, nil
}

// parseHTML parses an html/template wrapped in the layout and forces the
// contextual-escaping analysis now: html/template reports unsafe constructs
// (an action inside a script, an unquoted attribute...) only on first
// execution, which must not happen in production.
func parseHTML(name, layout, body string) (*htmltemplate.Template, error) {
	if reDataURI.MatchString(layout) || reDataURI.MatchString(body) {
		return nil, errors.New("data: URIs are not allowed in e-mail HTML; send the image as an inline CID attachment (Message.Inline) and reference it as cid:<CID>")
	}
	t := htmltemplate.New(layoutName).Funcs(htmlFuncs).Option("missingkey=error")
	if _, err := t.Parse(layout); err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	if _, err := t.New("content").Parse(body); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	// The probe fails on the first missing key; an escaping error is reported
	// before any data is read, so only *html/template.Error matters here.
	err := t.Execute(io.Discard, templateData{Data: map[string]any{}})
	var escErr *htmltemplate.Error
	if errors.As(err, &escErr) {
		return nil, err
	}
	return t, nil
}

// noticeData is what in_app/push texts are executed with: no Data, so no
// personal data can reach them.
type noticeData struct {
	Locale  string
	AppName string
}

// buildNotice renders the fixed title and body of one locale.
func buildNotice(id, loc string, parts map[layoutKey]string) (Notice, error) {
	var out [2]string
	for i, part := range noticeParts {
		src, ok := parts[layoutKey{loc, part}]
		if !ok {
			return Notice{}, fmt.Errorf("%s.%s.tmpl is missing", loc, part)
		}
		t, err := texttemplate.New(id + "." + loc + "." + part).Option("missingkey=error").Parse(src)
		if err != nil {
			return Notice{}, fmt.Errorf("%s %s: %w", loc, part, err)
		}
		var b bytes.Buffer
		if err := t.Execute(&b, noticeData{Locale: loc, AppName: appName}); err != nil {
			return Notice{}, fmt.Errorf("%s %s: in_app/push texts are fixed; only {{.AppName}} and {{.Locale}} are available: %w", loc, part, err)
		}
		out[i] = strings.TrimSpace(b.String())
	}
	title := strings.Join(strings.Fields(out[0]), " ")
	switch {
	case title == "":
		return Notice{}, fmt.Errorf("%s title is empty", loc)
	case utf8.RuneCountInString(title) > maxNoticeTitle:
		return Notice{}, fmt.Errorf("%s title is longer than %d characters", loc, maxNoticeTitle)
	case utf8.RuneCountInString(out[1]) > maxNoticeBody:
		return Notice{}, fmt.Errorf("%s body is longer than %d characters", loc, maxNoticeBody)
	}
	return Notice{Title: title, Body: out[1]}, nil
}

// formatDateTime renders t in Europe/Istanbul as "02.01.2006 15:04".
func formatDateTime(t time.Time) string { return t.In(clock.Istanbul()).Format("02.01.2006 15:04") }

// formatDate renders t in Europe/Istanbul as "02.01.2006".
func formatDate(t time.Time) string { return t.In(clock.Istanbul()).Format("02.01.2006") }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
