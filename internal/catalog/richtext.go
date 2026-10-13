package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"
)

// Rich text limits of an offering description (02 §18.2 description_json;
// 05 §14.6: Tiptap JSON with a size limit). The allow-list is the one of
// events (BE-024, internal/events/richtext.go): the same Tiptap subset is
// rendered by the same clients, and modules do not share unexported code.
const (
	maxDescriptionBytes = 64 << 10
	maxDescriptionDepth = 12
	maxDescriptionText  = 10000
)

// errRichText is the reason a description is refused (the handler turns it
// into a field error).
var errRichText = errors.New("catalog: invalid description")

// tiptapNode is one node of a Tiptap (ProseMirror) document.
type tiptapNode struct {
	Type    string         `json:"type"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Content []tiptapNode   `json:"content,omitempty"`
	Text    string         `json:"text,omitempty"`
	Marks   []tiptapMark   `json:"marks,omitempty"`
}

type tiptapMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// tiptapChildren is the node allow-list (05 §14.6: the same elements as the
// bluemonday policy — p, strong, em, ul, ol, li, br, h2, h3, blockquote, a)
// with the node types each one may contain.
var tiptapChildren = map[string][]string{
	"doc":         {"paragraph", "heading", "bulletList", "orderedList", "blockquote"},
	"paragraph":   {"text", "hardBreak"},
	"heading":     {"text", "hardBreak"},
	"bulletList":  {"listItem"},
	"orderedList": {"listItem"},
	"listItem":    {"paragraph", "bulletList", "orderedList"},
	"blockquote":  {"paragraph"},
	"text":        nil,
	"hardBreak":   nil,
}

// sanitizeDescription validates a Tiptap JSON description against the
// node/mark/attribute allow-list (05 §14.6) and returns its canonical
// encoding (only the allowed attributes survive). Raw HTML is never
// accepted.
func sanitizeDescription(raw []byte) (JSON, error) {
	if len(raw) > maxDescriptionBytes {
		return nil, errRichText
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc tiptapNode
	if err := dec.Decode(&doc); err != nil || dec.More() || doc.Type != "doc" {
		return nil, errRichText
	}
	var text strings.Builder
	clean, err := cleanNode(doc, 0, &text)
	if err != nil {
		return nil, err
	}
	if utf8.RuneCountInString(strings.TrimSpace(text.String())) > maxDescriptionText {
		return nil, errRichText
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return nil, errRichText
	}
	return JSON(out), nil
}

func cleanNode(n tiptapNode, depth int, text *strings.Builder) (tiptapNode, error) {
	allowed, known := tiptapChildren[n.Type]
	if !known || depth > maxDescriptionDepth {
		return tiptapNode{}, errRichText
	}
	out := tiptapNode{Type: n.Type}
	switch n.Type {
	case "text":
		if n.Text == "" || len(n.Content) > 0 || !utf8.ValidString(n.Text) {
			return tiptapNode{}, errRichText
		}
		marks, err := cleanMarks(n.Marks)
		if err != nil {
			return tiptapNode{}, err
		}
		out.Text, out.Marks = n.Text, marks
		text.WriteString(n.Text)
		return out, nil
	case "heading":
		lvl, ok := n.Attrs["level"].(float64)
		if !ok || (lvl != 2 && lvl != 3) {
			return tiptapNode{}, errRichText
		}
		out.Attrs = map[string]any{"level": int(lvl)}
	case "orderedList":
		if s, ok := n.Attrs["start"]; ok {
			f, isNum := s.(float64)
			if !isNum || f < 1 || f > 10000 || f != float64(int(f)) {
				return tiptapNode{}, errRichText
			}
			out.Attrs = map[string]any{"start": int(f)}
		}
	case "hardBreak":
		text.WriteString("\n")
	}
	if n.Text != "" || len(n.Marks) > 0 {
		return tiptapNode{}, errRichText
	}
	for _, c := range n.Content {
		if !contains(allowed, c.Type) {
			return tiptapNode{}, errRichText
		}
		cc, err := cleanNode(c, depth+1, text)
		if err != nil {
			return tiptapNode{}, err
		}
		out.Content = append(out.Content, cc)
	}
	if n.Type == "paragraph" || n.Type == "heading" {
		text.WriteString("\n")
	}
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// cleanMarks keeps bold, italic and link (https/mailto href only; 05 §14.6).
func cleanMarks(ms []tiptapMark) ([]tiptapMark, error) {
	var out []tiptapMark
	for _, m := range ms {
		switch m.Type {
		case "bold", "italic":
			out = append(out, tiptapMark{Type: m.Type})
		case "link":
			href, _ := m.Attrs["href"].(string)
			if !safeHref(href) {
				return nil, errRichText
			}
			out = append(out, tiptapMark{Type: "link", Attrs: map[string]any{"href": href}})
		default:
			return nil, errRichText
		}
	}
	return out, nil
}

func safeHref(h string) bool {
	if h == "" || len(h) > 2048 {
		return false
	}
	u, err := url.Parse(h)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "https":
		return u.Host != "" && u.User == nil
	case "mailto":
		return u.Opaque != ""
	}
	return false
}
