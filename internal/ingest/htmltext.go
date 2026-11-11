package ingest

import (
	"bytes"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// The HTML helpers of the adapters: a parsed page, its visible text (the
// extractor's input) and its links, all over golang.org/x/net/html. Nothing
// of the page is executed or rendered; scripts, styles, forms and media are
// skipped.

// skipped are the elements whose content is never text.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Noscript: true, atom.Template: true, atom.Svg: true, atom.Iframe: true,
	atom.Head: true, atom.Form: true, atom.Select: true, atom.Button: true, atom.Video: true, atom.Audio: true,
	atom.Canvas: true, atom.Object: true,
}

// chrome are the page frame elements left out of the main text.
var chrome = map[atom.Atom]bool{atom.Nav: true, atom.Footer: true, atom.Header: true, atom.Aside: true}

// blocks end a line in the text.
var blocks = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Br: true, atom.Li: true, atom.Tr: true, atom.Td: true, atom.Th: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.Section: true,
	atom.Article: true, atom.Table: true, atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Dt: true, atom.Dd: true,
	atom.Blockquote: true, atom.Pre: true, atom.Hr: true, atom.Time: true, atom.Address: true, atom.Figcaption: true,
}

// parseHTML parses a page body.
func parseHTML(body []byte) (*html.Node, error) { return html.Parse(bytes.NewReader(body)) }

// mainNode returns the page's <main>, else its first <article>, else
// <body>, else the root.
func mainNode(root *html.Node) *html.Node {
	for _, a := range []atom.Atom{atom.Main, atom.Article, atom.Body} {
		if n := findFirst(root, func(n *html.Node) bool { return n.DataAtom == a }); n != nil {
			return n
		}
	}
	return root
}

// findFirst returns the first element (document order) matching ok.
func findFirst(n *html.Node, ok func(*html.Node) bool) *html.Node {
	if n.Type == html.ElementNode && ok(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findFirst(c, ok); f != nil {
			return f
		}
	}
	return nil
}

// findAll returns every element under n matching ok, in document order
// (a match's descendants are searched too).
func findAll(n *html.Node, ok func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && ok(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// attr returns an attribute of n ("" when absent).
func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hasClass reports whether n carries class c.
func hasClass(n *html.Node, c string) bool {
	return n.Type == html.ElementNode && containsWord(attr(n, "class"), c)
}

func containsWord(list, w string) bool {
	for _, f := range strings.Fields(list) {
		if f == w {
			return true
		}
	}
	return false
}

// byClass matches the elements carrying class c.
func byClass(c string) func(*html.Node) bool {
	return func(n *html.Node) bool { return hasClass(n, c) }
}

// byAtom matches the elements of one tag.
func byAtom(a atom.Atom) func(*html.Node) bool {
	return func(n *html.Node) bool { return n.DataAtom == a }
}

// nodeText returns the visible text under n, block elements ending a line,
// spaces collapsed, empty lines dropped. withChrome keeps nav/header/footer.
func nodeText(top *html.Node, withChrome bool) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
			return
		case html.ElementNode:
			if n != top && (skipped[n.DataAtom] || (!withChrome && chrome[n.DataAtom])) {
				return // the node asked for is read whatever its tag (a button's label)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blocks[n.DataAtom] {
			b.WriteByte('\n')
		}
	}
	walk(top)
	return tidyLines(b.String())
}

// inlineText is the text of an inline element on one line.
func inlineText(n *html.Node) string { return strings.Join(strings.Fields(nodeText(n, true)), " ") }

// tidyLines collapses the spaces of every line and drops empty lines.
func tidyLines(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, " ", " "), "\n")
	out := lines[:0]
	for _, l := range lines {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// resolve returns href against base as an absolute http(s) URL without a
// fragment ("" for other schemes, mailto: included, or a malformed one).
func resolve(base *url.URL, href string) string {
	href = strings.TrimSpace(href)
	if href == "" || strings.HasPrefix(href, "#") {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return ""
	}
	u.Fragment = ""
	return u.String()
}
