package domainevent_test

import (
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"uuid"

	"koro/internal/platform/civil"
	"koro/internal/platform/domainevent"
)

// forbiddenWords may not appear as a word of a payload field name or JSON
// key (01 §0-6, §5.4): personal data and talent values never enter an event.
var forbiddenWords = map[string]bool{
	"email": true, "mail": true, "name": true, "names": true, "phone": true, "answer": true, "answers": true,
	"token": true, "band": true, "score": true, "password": true, "iban": true, "address": true, "tckn": true,
	"body": true, "text": true, "message": true, "comment": true, "note": true,
}

var (
	uuidType  = reflect.TypeFor[uuid.UUID]()
	timeType  = reflect.TypeFor[time.Time]()
	civilType = reflect.TypeFor[civil.Date]()
)

// allowedType reports the field types a payload may carry: identifiers,
// codes, flags, counts, instants and lists of codes or identifiers.
func allowedType(t reflect.Type) bool {
	switch {
	case t == uuidType, t == timeType, t == civilType:
		return true
	case t.Kind() == reflect.Pointer:
		return t.Elem() == uuidType || t.Elem() == timeType
	case t.Kind() == reflect.Slice:
		return t.Elem() == uuidType || t.Elem().Kind() == reflect.String
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool, reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// camelWords splits "StaffUserID" into staff, user, id.
func camelWords(s string) []string {
	var out []string
	start := 0
	r := []rune(s)
	for i := 1; i <= len(r); i++ {
		if i == len(r) || (unicode.IsUpper(r[i]) && (unicode.IsLower(r[i-1]) || (i+1 < len(r) && unicode.IsLower(r[i+1])))) {
			out = append(out, strings.ToLower(string(r[start:i])))
			start = i
		}
	}
	return out
}

// payloadViolations checks one payload struct type.
func payloadViolations(t reflect.Type) []string {
	var v []string
	if t.Kind() != reflect.Struct {
		return []string{t.String() + ": payload must be a struct"}
	}
	for i := range t.NumField() {
		f := t.Field(i)
		where := t.Name() + "." + f.Name
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if !allowedType(f.Type) {
			v = append(v, where+": type "+f.Type.String()+" is not an identifier, code, flag, count or instant")
		}
		if strings.HasPrefix(f.Name, "Talent") || strings.HasPrefix(tag, "talent") {
			v = append(v, where+": talent values never enter an event (01 §0-7)")
			continue
		}
		identifier := strings.HasSuffix(f.Name, "ID") && (f.Type == uuidType || f.Type == reflect.PointerTo(uuidType))
		if identifier {
			continue
		}
		words := append(camelWords(f.Name), strings.Split(tag, "_")...)
		for _, w := range words {
			if forbiddenWords[w] {
				v = append(v, where+": forbidden word "+strconv.Quote(w)+" (personal data, 01 §5.4)")
				break
			}
		}
	}
	return v
}

func TestDomainevent_PayloadsHaveNoPIIFields(t *testing.T) {
	t.Parallel()
	for _, s := range domainevent.Catalog() {
		if v := payloadViolations(reflect.TypeOf(s.Payload)); len(v) > 0 {
			t.Errorf("%s:\n%s", s.Topic, strings.Join(v, "\n"))
		}
	}

	// The rule itself catches every forbidden shape.
	type leaky struct {
		UserID        uuid.UUID
		TokenID       uuid.UUID // an identifier: allowed
		Email         string
		FullName      string
		PhoneNumber   string
		Answers       []string
		Token         string
		TalentPoints  int
		Band          string
		Score         int
		Extra         map[string]any
		Raw           json.RawMessage
		Note          string `json:"note"`
		Ok            string `json:"contact_email"`
		TalentScoreID uuid.UUID
	}
	got := payloadViolations(reflect.TypeFor[leaky]())
	for _, field := range []string{"Email", "FullName", "PhoneNumber", "Answers", "Token", "TalentPoints", "Band", "Score",
		"Extra", "Raw", "Note", "Ok", "TalentScoreID"} {
		if !slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "leaky."+field+":") }) {
			t.Errorf("rule misses leaky.%s; violations: %q", field, got)
		}
	}
	for _, s := range got {
		if strings.HasPrefix(s, "leaky.UserID:") || strings.HasPrefix(s, "leaky.TokenID:") {
			t.Errorf("identifier flagged: %s", s)
		}
	}
}

// pascal maps "order_line.fulfillment_marked" to "OrderLineFulfillmentMarked"
// (dsr → DSR).
func pascal(topic string) string {
	var b strings.Builder
	for _, p := range strings.FieldsFunc(topic, func(r rune) bool { return r == '.' || r == '_' }) {
		if p == "dsr" {
			b.WriteString("DSR")
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

// snake maps "StaffUserID" to "staff_user_id" and "DSRID" to "dsr_id".
func snake(field string) string {
	if strings.HasPrefix(field, "DSR") {
		return "dsr_" + snake(field[3:])
	}
	return strings.Join(camelWords(field), "_")
}

func TestDomainevent_NamesFollowTheTopic(t *testing.T) {
	t.Parallel()
	cat := domainevent.Catalog()
	if len(cat) != 71 {
		t.Fatalf("catalog has %d topics, want the 71 of 01 §6.2", len(cat))
	}
	for _, s := range cat {
		typ := reflect.TypeOf(s.Payload)
		if typ.Name() != pascal(s.Topic) {
			t.Errorf("%s: payload type %s, want %s", s.Topic, typ.Name(), pascal(s.Topic))
		}
		if typ.PkgPath() != reflect.TypeFor[domainevent.Envelope]().PkgPath() {
			t.Errorf("%s: payload type lives outside domainevent", s.Topic)
		}
		if s.Version != 1 {
			t.Errorf("%s: version %d, want 1 until a breaking change", s.Topic, s.Version)
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			if got := f.Tag.Get("json"); got != snake(f.Name) {
				t.Errorf("%s.%s: json tag %q, want %q", typ.Name(), f.Name, got, snake(f.Name))
			}
		}
	}
}

// docRow is one parsed row of 01 §6.2.
type docRow struct {
	producer, mechanism string
	fields              []string
}

// TestDomainevent_CatalogMatchesDesignDoc re-reads 01 §6.2 when it is
// reachable (koro-files checkout; CI has only the repo) and compares
// topics, producers, mechanisms and payload field names.
func TestDomainevent_CatalogMatchesDesignDoc(t *testing.T) {
	t.Parallel()
	doc := findDesignDoc(t)
	if doc == "" {
		t.Skip("docs/design/01-moduller.md not reachable from the repo (CI checkout)")
	}
	b, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	topicRe := regexp.MustCompile("`([a-z_]+\\.[a-z_]+)`")
	parens := regexp.MustCompile(`\s*\([^)]*\)`)
	want := map[string]docRow{}
	in := false
	for ln := range strings.SplitSeq(string(b), "\n") {
		switch {
		case strings.HasPrefix(ln, "### 6.2 "):
			in = true
			continue
		case strings.HasPrefix(ln, "### 6.3 "):
			in = false
		}
		if !in || !strings.HasPrefix(ln, "| `") {
			continue
		}
		cells := strings.Split(ln, "|")
		if len(cells) < 7 {
			t.Fatalf("01 §6.2: malformed row %q", ln)
		}
		row := docRow{
			producer:  strings.Fields(strings.TrimSpace(cells[2]))[0],
			mechanism: strings.TrimSpace(cells[4]),
		}
		for f := range strings.SplitSeq(parens.ReplaceAllString(cells[5], ""), ",") {
			row.fields = append(row.fields, strings.TrimSpace(f))
		}
		for _, m := range topicRe.FindAllStringSubmatch(cells[1], -1) {
			want[m[1]] = row
		}
	}
	if len(want) == 0 {
		t.Fatal("01 §6.2: no topic rows found")
	}
	got := map[string]bool{}
	for _, s := range domainevent.Catalog() {
		got[s.Topic] = true
		w, ok := want[s.Topic]
		if !ok {
			t.Errorf("%s: not in 01 §6.2 (add it to the design first)", s.Topic)
			continue
		}
		if s.Producer != w.producer || string(s.Mechanism) != w.mechanism {
			t.Errorf("%s: producer/mechanism %s/%s, 01 §6.2 says %s/%s", s.Topic, s.Producer, s.Mechanism, w.producer, w.mechanism)
		}
		typ := reflect.TypeOf(s.Payload)
		var fields []string
		for i := range typ.NumField() {
			fields = append(fields, typ.Field(i).Name)
		}
		if !slices.Equal(fields, w.fields) {
			t.Errorf("%s: payload fields %v, 01 §6.2 says %v", s.Topic, fields, w.fields)
		}
	}
	for topic := range want {
		if !got[topic] {
			t.Errorf("%s: in 01 §6.2 but not in the catalog", topic)
		}
	}
}

func findDesignDoc(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 9 {
		p := filepath.Join(dir, "docs", "design", "01-moduller.md")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		dir = filepath.Dir(dir)
	}
	return ""
}

func TestDomainevent_EnvelopeRoundTrip(t *testing.T) {
	t.Parallel()
	reg, ev, user := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	in := domainevent.RegistrationConfirmed{RegistrationID: reg, EventID: ev, UserID: user, Sensitive: true, TicketVersion: 2}
	e, err := domainevent.New(in, "event_registration", reg)
	if err != nil {
		t.Fatal(err)
	}
	if e.Topic != "registration.confirmed" || e.Version != 1 || e.AggregateID != reg || e.AggregateType != "event_registration" {
		t.Fatalf("envelope = %+v", e)
	}
	if !strings.Contains(string(e.Payload), `"registration_id":"`+reg.String()+`"`) {
		t.Fatalf("payload = %s", e.Payload)
	}

	// The envelope survives a JSON round trip (the task payload).
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var back domainevent.Envelope
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	out, err := domainevent.Decode[domainevent.RegistrationConfirmed](back)
	if err != nil || out != in {
		t.Fatalf("Decode() = %+v, %v; want %+v", out, err, in)
	}

	if _, err := domainevent.Decode[domainevent.EventCancelled](back); !errors.Is(err, domainevent.ErrTopicMismatch) {
		t.Fatalf("Decode(wrong type) error = %v", err)
	}
	if _, err := domainevent.New(unknownPayload{}, "x", reg); !errors.Is(err, domainevent.ErrUnknownTopic) {
		t.Fatalf("New(unknown topic) error = %v", err)
	}
	// Fields added by a newer producer are ignored.
	back.Payload = json.RawMessage(`{"event_id":"` + ev.String() + `","org_id":"` + ev.String() + `","added_later":1}`)
	back.Topic = domainevent.TopicEventCancelled
	if c, err := domainevent.Decode[domainevent.EventCancelled](back); err != nil || c.EventID != ev {
		t.Fatalf("Decode(with unknown field) = %+v, %v", c, err)
	}
	if s, ok := domainevent.Lookup(domainevent.TopicUserActiveDay); !ok || s.Mechanism != domainevent.AfterCommit || s.Producer != "identity" {
		t.Fatalf("Lookup(user.active_day) = %+v, %v", s, ok)
	}
}

type unknownPayload struct{}

func (unknownPayload) Topic() string { return "nobody.knows" }

// TestDomainevent_IsALeaf: producers and consumers import domainevent, so it
// may import only the standard library and platform/civil (01 §3.2).
func TestDomainevent_IsALeaf(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.Split(p, "/")[0], ".") || (strings.HasPrefix(p, "koro/") && p != "koro/internal/platform/civil") {
				t.Errorf("%s imports %s", f, p)
			}
		}
	}
}
