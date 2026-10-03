// Package sanitize is the second line of defence for HTML (05 §14.6,
// R-002 §5.5). Rich text is accepted as Tiptap JSON and validated against
// an allow list (first line); output is escaped with html/template. Where
// HTML cannot be avoided, it goes through the single bluemonday policy
// here: a few formatting elements, https/mailto links with rel="nofollow",
// no scripts, styles, event handlers, images or embeds.
//
// The package is a leaf (standard library and bluemonday only). Keep
// golang.org/x/net current: bluemonday parses with x/net/html.
package sanitize

import (
	"sync"

	"github.com/microcosm-cc/bluemonday"
)

// policy is built once; a configured bluemonday.Policy is safe for
// concurrent use.
var policy = sync.OnceValue(func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowElements("p", "strong", "em", "ul", "ol", "li", "br", "h2", "h3", "blockquote", "a")
	p.AllowAttrs("href").OnElements("a")
	p.AllowURLSchemes("https", "mailto")
	p.RequireNoFollowOnLinks(true)
	p.RequireParseableURLs(true)
	return p
})

// HTML returns s with everything outside the allow list removed.
func HTML(s string) string { return policy().Sanitize(s) }
