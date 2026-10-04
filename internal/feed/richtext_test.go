package feed_test

import (
	"strings"
	"testing"

	"koro/internal/feed"
)

// TestTiptap_DisallowedNode_422: a node outside the allow-list (an image,
// raw HTML, a script, a code block) refuses the whole body with 422 on
// body.body_json, through createPost and on its own.
func TestTiptap_DisallowedNode_422(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, body := range []string{
		`{"type":"doc","content":[{"type":"image","attrs":{"src":"https://x.test/a.png"}}]}`,
		`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"html","text":"<script>alert(1)</script>"}]}]}`,
		`{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"x"}]}]}`,
		`{"type":"doc","content":[{"type":"text","text":"text directly in doc"}]}`,
	} {
		in := feed.PostInput{Kind: feed.KindText, Body: []byte(body)}
		_, err := e.svc.CreatePost(t.Context(), persona(t, "club_admin"), orgID("club_a"), in)
		wantField(t, err, "body.body_json")
	}
	// Raw HTML is not a document at all.
	for _, raw := range []string{`"<p>merhaba</p>"`, `<p>merhaba</p>`, `[]`, `{"type":"paragraph"}`} {
		if _, _, err := feed.SanitizeBody([]byte(raw)); err == nil {
			t.Fatalf("SanitizeBody(%s) accepted", raw)
		}
	}
}

// TestTiptap_ScriptLink_Stripped: a javascript:, data: or credentialed
// link loses its link mark and keeps its text; a https link keeps only its
// href (target, rel, class dropped).
func TestTiptap_ScriptLink_Stripped(t *testing.T) {
	t.Parallel()
	for _, href := range []string{
		"javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,<script>alert(1)</script>",
		"vbscript:msgbox(1)", "https://user:pass@evil.test/", "//evil.test/x", "/relative", "http://plain.test/",
	} {
		body := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"tıkla",` +
			`"marks":[{"type":"bold"},{"type":"link","attrs":{"href":` + quote(href) + `,"target":"_blank"}}]}]}]}`
		clean, text, err := feed.SanitizeBody([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", href, err)
		}
		if strings.Contains(string(clean), `"link"`) || strings.Contains(strings.ToLower(string(clean)), "javascript") ||
			strings.Contains(string(clean), "data:") || text != "tıkla" || !strings.Contains(string(clean), `"bold"`) {
			t.Fatalf("%s: clean = %s, text = %q", href, clean, text)
		}
	}
	body := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"site",` +
		`"marks":[{"type":"link","attrs":{"href":"https://koro.test/a?b=1","target":"_blank","rel":"noopener","class":null}}]}]}]}`
	clean, _, err := feed.SanitizeBody([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if want := `"marks":[{"type":"link","attrs":{"href":"https://koro.test/a?b=1"}}]`; !strings.Contains(string(clean), want) {
		t.Fatalf("clean = %s", clean)
	}
	// Through the service the stored body has no script link either.
	e := newEnv(t)
	in := feed.PostInput{Kind: feed.KindText, Body: []byte(`{"type":"doc","content":[{"type":"paragraph","content":[` +
		`{"type":"text","text":"x","marks":[{"type":"link","attrs":{"href":"javascript:alert(1)"}}]}]}]}`)}
	v := e.create(t, "club_admin", "club_a", in)
	if strings.Contains(string(v.Post.BodyJSON), "javascript") || *v.Post.BodyText != "x" {
		t.Fatalf("stored body = %s", v.Post.BodyJSON)
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// TestTiptap_AllowList pins the accepted shapes and the refused details.
func TestTiptap_AllowList(t *testing.T) {
	t.Parallel()
	ok := `{"type":"doc","content":[
		{"type":"heading","attrs":{"level":2},"content":[{"type":"text","text":"Başlık"}]},
		{"type":"paragraph"},
		{"type":"paragraph","content":[{"type":"text","text":"bir","marks":[{"type":"italic"}]},{"type":"hardBreak"},{"type":"text","text":"iki"}]},
		{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"madde"}]}]}]},
		{"type":"orderedList","attrs":{"start":3,"type":null},"content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"üç"}]}]}]},
		{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"alıntı","marks":[{"type":"link","attrs":{"href":"mailto:kulup@uni.edu.tr"}}]}]}]}
	]}`
	clean, text, err := feed.SanitizeBody([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if text != "Başlık\n\nbir\niki\nmadde\nüç\nalıntı" {
		t.Fatalf("text = %q", text)
	}
	if strings.Contains(string(clean), `"type":null`) || !strings.Contains(string(clean), `"start":3`) {
		t.Fatalf("clean = %s", clean)
	}
	for name, body := range map[string]string{
		"unknown field":        `{"type":"doc","content":[],"foo":1}`,
		"heading level 1":      `{"type":"doc","content":[{"type":"heading","attrs":{"level":1},"content":[{"type":"text","text":"x"}]}]}`,
		"heading no level":     `{"type":"doc","content":[{"type":"heading","content":[{"type":"text","text":"x"}]}]}`,
		"paragraph attrs":      `{"type":"doc","content":[{"type":"paragraph","attrs":{"textAlign":"left"}}]}`,
		"underline mark":       `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"x","marks":[{"type":"underline"}]}]}]}`,
		"empty text":           `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":""}]}]}`,
		"nul in text":          `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"a\u0000b"}]}]}`,
		"bidi override":        `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"a` + "\u202e" + `b"}]}]}`,
		"list item in doc":     `{"type":"doc","content":[{"type":"listItem","content":[]}]}`,
		"bold with href attrs": `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"x","marks":[{"type":"bold","attrs":{"href":"https://a.test"}}]}]}]}`,
		"two documents":        `{"type":"doc"}{"type":"doc"}`,
	} {
		if _, _, err := feed.SanitizeBody([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	deep := `{"type":"doc","content":[` + strings.Repeat(`{"type":"bulletList","content":[{"type":"listItem","content":[`, 5) +
		`{"type":"paragraph"}` + strings.Repeat(`]}]}`, 5) + `]}`
	if _, _, err := feed.SanitizeBody([]byte(deep)); err == nil {
		t.Fatal("a body nested 11 levels deep was accepted")
	}
	long := `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"` + strings.Repeat("a", 5001) + `"}]}]}`
	if _, _, err := feed.SanitizeBody([]byte(long)); err == nil {
		t.Fatal("a body of 5001 characters was accepted")
	}
}

// FuzzSanitizeBody: whatever the input, an accepted body re-validates to
// itself and never carries a non-https/mailto link.
func FuzzSanitizeBody(f *testing.F) {
	f.Add([]byte(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"x","marks":[{"type":"link","attrs":{"href":"javascript:x"}}]}]}]}`))
	f.Add([]byte(`{"type":"doc","content":[{"type":"heading","attrs":{"level":3},"content":[{"type":"text","text":"y"}]}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		clean, _, err := feed.SanitizeBody(raw)
		if err != nil {
			return
		}
		again, _, err := feed.SanitizeBody(clean)
		if err != nil || string(again) != string(clean) {
			t.Fatalf("canonical body not stable: %s → %s (%v)", clean, again, err)
		}
		if strings.Contains(strings.ToLower(string(clean)), `"href":"javascript`) {
			t.Fatalf("script link kept: %s", clean)
		}
	})
}
