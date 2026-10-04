package feed

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of a post body (02 §12.2 body_text ≤ 5000; R-003 §1.5.4: ≤ 64 KB,
// depth ≤ 8, ≤ 2000 nodes).
const (
	maxBodyBytes = 64 << 10
	maxBodyDepth = 8
	maxBodyNodes = 2000
	maxBodyText  = 5000
	maxHrefLen   = 2048
)

// Reasons a body is refused (the service turns them into a 422 field
// error on body.body_json; the message never echoes the content).
var (
	errBodyShape     = errors.New("must be a Tiptap document (type doc) of at most 64 KB without unknown fields")
	errBodyNode      = errors.New("contains a node type that is not allowed")
	errBodyNesting   = errors.New("contains a node where it is not allowed")
	errBodyMark      = errors.New("contains a mark that is not allowed")
	errBodyAttrs     = errors.New("contains an attribute that is not allowed")
	errBodyText      = errors.New("contains an invalid text node")
	errBodyTooDeep   = errors.New("is nested too deeply (at most 8 levels)")
	errBodyTooBig    = errors.New("has too many nodes (at most 2000)")
	errBodyTextLimit = errors.New("plain text exceeds 5000 characters")
)

// tiptapNode is the closed input type of a Tiptap (ProseMirror) node: a
// field outside it fails the decoding (DisallowUnknownFields applies to
// every nested struct), so nothing unknown is ever stored (05 §14.6,
// R-003 §1.5.4).
type tiptapNode struct {
	Type    string       `json:"type"`
	Attrs   *nodeAttrs   `json:"attrs,omitempty"`
	Content []tiptapNode `json:"content,omitempty"`
	Text    *string      `json:"text,omitempty"`
	Marks   []tiptapMark `json:"marks,omitempty"`
}

// nodeAttrs are the attributes Tiptap emits on the allowed nodes: level on
// a heading, start (and type, dropped) on an ordered list.
type nodeAttrs struct {
	Level *int    `json:"level,omitempty"`
	Start *int    `json:"start,omitempty"`
	Type  *string `json:"type,omitempty"`
}

type tiptapMark struct {
	Type  string     `json:"type"`
	Attrs *markAttrs `json:"attrs,omitempty"`
}

// markAttrs are the attributes of Tiptap's link mark. Only href survives;
// target, rel and class are dropped (clients render links with fixed
// rel="noopener noreferrer nofollow ugc" and target="_blank").
type markAttrs struct {
	Href   *string `json:"href,omitempty"`
	Target *string `json:"target,omitempty"`
	Rel    *string `json:"rel,omitempty"`
	Class  *string `json:"class,omitempty"`
}

// cleanNode is the canonical output node: only the allowed attributes.
type cleanNode struct {
	Type    string         `json:"type"`
	Attrs   map[string]int `json:"attrs,omitempty"`
	Content []cleanNode    `json:"content,omitempty"`
	Text    string         `json:"text,omitempty"`
	Marks   []cleanMark    `json:"marks,omitempty"`
}

type cleanMark struct {
	Type  string            `json:"type"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// allowedChildren is the node allow-list (05 §14.6: the elements of the
// bluemonday policy — p, strong, em, ul, ol, li, br, h2, h3, blockquote, a)
// with the node types each one may contain. Images are never in the body:
// they are post_media (R-003 §1.5.4).
var allowedChildren = map[string][]string{
	"doc":         {"paragraph", "heading", "bulletList", "orderedList", "blockquote"},
	"paragraph":   {"text", "hardBreak"},
	"heading":     {"text", "hardBreak"},
	"bulletList":  {"listItem"},
	"orderedList": {"listItem"},
	"listItem":    {"paragraph", "bulletList", "orderedList"},
	"blockquote":  {"paragraph", "bulletList", "orderedList"},
	"text":        nil,
	"hardBreak":   nil,
}

// sanitizeBody validates a Tiptap JSON body against the closed node / mark
// / attribute allow-list and returns its canonical encoding and its plain
// text (the body_text column: preview and search). Raw HTML is never
// accepted (a string is not a document). A link whose href is not https://
// or mailto: (javascript:, data:, credentials in the URL, ...) loses its
// link mark and keeps its text; any other unknown node, mark or attribute
// refuses the whole body.
func sanitizeBody(raw []byte) (JSON, string, error) {
	if len(raw) == 0 || len(raw) > maxBodyBytes {
		return nil, "", errBodyShape
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc tiptapNode
	if err := dec.Decode(&doc); err != nil || dec.More() || doc.Type != "doc" {
		return nil, "", errBodyShape
	}
	w := walker{}
	clean, err := w.node(doc, 0)
	if err != nil {
		return nil, "", err
	}
	plain := strings.TrimSpace(w.text.String())
	if utf8.RuneCountInString(plain) > maxBodyText {
		return nil, "", errBodyTextLimit
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return nil, "", errBodyShape
	}
	return JSON(out), plain, nil
}

type walker struct {
	text  strings.Builder
	nodes int
}

func (w *walker) node(n tiptapNode, depth int) (cleanNode, error) {
	allowed, known := allowedChildren[n.Type]
	switch {
	case !known:
		return cleanNode{}, errBodyNode
	case depth > maxBodyDepth:
		return cleanNode{}, errBodyTooDeep
	}
	if w.nodes++; w.nodes > maxBodyNodes {
		return cleanNode{}, errBodyTooBig
	}
	out := cleanNode{Type: n.Type}
	attrs, err := nodeAttributes(n)
	if err != nil {
		return cleanNode{}, err
	}
	out.Attrs = attrs
	if n.Type == "text" {
		if n.Text == nil || *n.Text == "" || !validText(*n.Text) || len(n.Content) > 0 {
			return cleanNode{}, errBodyText
		}
		marks, err := cleanMarks(n.Marks)
		if err != nil {
			return cleanNode{}, err
		}
		out.Text, out.Marks = *n.Text, marks
		w.text.WriteString(*n.Text)
		return out, nil
	}
	if n.Text != nil || len(n.Marks) > 0 {
		return cleanNode{}, errBodyText
	}
	if n.Type == "hardBreak" {
		w.text.WriteString("\n")
	}
	for _, c := range n.Content {
		if !contains(allowed, c.Type) {
			if _, ok := allowedChildren[c.Type]; !ok {
				return cleanNode{}, errBodyNode
			}
			return cleanNode{}, errBodyNesting
		}
		cc, err := w.node(c, depth+1)
		if err != nil {
			return cleanNode{}, err
		}
		out.Content = append(out.Content, cc)
	}
	if n.Type == "paragraph" || n.Type == "heading" {
		w.text.WriteString("\n")
	}
	return out, nil
}

// nodeAttributes keeps heading.level (2 or 3) and orderedList.start
// (1–10000); orderedList.type is dropped. Any attribute on another node,
// or out of range, refuses the body.
func nodeAttributes(n tiptapNode) (map[string]int, error) {
	a := n.Attrs
	if a == nil {
		if n.Type == "heading" {
			return nil, errBodyAttrs // a heading needs its level
		}
		return nil, nil
	}
	switch n.Type {
	case "heading":
		if a.Level == nil || (*a.Level != 2 && *a.Level != 3) || a.Start != nil || a.Type != nil {
			return nil, errBodyAttrs
		}
		return map[string]int{"level": *a.Level}, nil
	case "orderedList":
		if a.Level != nil {
			return nil, errBodyAttrs
		}
		if a.Start == nil {
			return nil, nil
		}
		if *a.Start < 1 || *a.Start > 10000 {
			return nil, errBodyAttrs
		}
		return map[string]int{"start": *a.Start}, nil
	}
	if a.Level != nil || a.Start != nil || a.Type != nil {
		return nil, errBodyAttrs
	}
	return nil, nil
}

// cleanMarks keeps bold, italic and safe links (05 §14.6); an unsafe link
// is stripped (its text stays), duplicates collapse, anything else refuses
// the body.
func cleanMarks(ms []tiptapMark) ([]cleanMark, error) {
	var out []cleanMark
	seen := map[string]bool{}
	for _, m := range ms {
		var c cleanMark
		switch m.Type {
		case "bold", "italic":
			if m.Attrs != nil && (m.Attrs.Href != nil || m.Attrs.Target != nil || m.Attrs.Rel != nil || m.Attrs.Class != nil) {
				return nil, errBodyAttrs
			}
			c = cleanMark{Type: m.Type}
		case "link":
			href := ""
			if m.Attrs != nil && m.Attrs.Href != nil {
				href = strings.TrimSpace(*m.Attrs.Href)
			}
			if !safeHref(href) {
				continue // stripped: the text stays without the link
			}
			c = cleanMark{Type: "link", Attrs: map[string]string{"href": href}}
		default:
			return nil, errBodyMark
		}
		if seen[c.Type] {
			continue
		}
		seen[c.Type] = true
		out = append(out, c)
	}
	return out, nil
}

// safeHref accepts https:// links with a host and no credentials, and
// mailto: addresses; everything else (javascript:, data:, vbscript:,
// relative and protocol-relative URLs) is unsafe.
func safeHref(h string) bool {
	if h == "" || len(h) > maxHrefLen || !validText(h) || strings.ContainsAny(h, " \t\r\n\\") {
		return false
	}
	u, err := url.Parse(h)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Scheme == "https" && strings.HasPrefix(h, "https://") && u.Host != "" && u.User == nil
	case "mailto":
		return u.Scheme == "mailto" && u.Opaque != "" && !strings.ContainsAny(u.Opaque, "<>\"")
	}
	return false
}

// validText refuses invalid UTF-8, NUL and the other C0/C1 controls
// (except tab) and bidirectional overrides (Trojan Source style text).
func validText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		switch {
		case r == '\t':
		case unicode.IsControl(r):
			return false
		case r >= '\u202a' && r <= '\u202e', r >= '\u2066' && r <= '\u2069':
			return false
		}
	}
	return true
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}
