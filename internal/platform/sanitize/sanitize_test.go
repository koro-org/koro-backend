package sanitize_test

import (
	"strings"
	"testing"

	"koro/internal/platform/sanitize"
)

func TestSanitize_StripsScript(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in    string
		want        string   // exact output when not empty
		mustNot     []string // substrings that must not survive (case-insensitive)
		mustContain []string
	}{
		{name: "script element", in: `<p>Merhaba</p><script>alert(1)</script>`, want: `<p>Merhaba</p>`},
		{name: "script in attribute context", in: `<p onclick="alert(1)">Tıkla</p>`, want: `<p>Tıkla</p>`},
		{name: "img onerror", in: `<img src=x onerror=alert(1)>Resim`, want: `Resim`},
		{name: "javascript link", in: `<a href="javascript:alert(1)">bağlantı</a>`, mustNot: []string{"javascript", "alert"}, mustContain: []string{"bağlantı"}},
		{name: "obfuscated javascript link", in: `<a href="jav&#x09;ascript:alert(1)">x</a>`, mustNot: []string{"ascript", "alert"}},
		{name: "data URL", in: `<a href="data:text/html;base64,PHNjcmlwdD4=">x</a>`, mustNot: []string{"data:"}},
		{name: "plain http link dropped", in: `<a href="http://koro.test">x</a>`, mustNot: []string{"http://"}},
		{name: "https link gets nofollow", in: `<a href="https://koro.test/a">site</a>`, want: `<a href="https://koro.test/a" rel="nofollow">site</a>`},
		{name: "mailto allowed", in: `<a href="mailto:destek@koro.test">e-posta</a>`, mustContain: []string{`href="mailto:destek@koro.test"`}},
		{name: "style and iframe", in: `<style>p{}</style><iframe src="https://x.test"></iframe><p>ok</p>`, want: `<p>ok</p>`},
		{name: "svg onload", in: `<svg onload=alert(1)><p>ok</p></svg>`, want: `<p>ok</p>`},
		{name: "allowed formatting kept", in: `<h2>Başlık</h2><ul><li><strong>a</strong> <em>b</em></li></ul><blockquote>c</blockquote><br>`,
			want: `<h2>Başlık</h2><ul><li><strong>a</strong> <em>b</em></li></ul><blockquote>c</blockquote><br>`},
		{name: "text is escaped", in: `1 < 2 & "x"`, want: `1 &lt; 2 &amp; &#34;x&#34;`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sanitize.HTML(tt.in)
			if tt.want != "" && got != tt.want {
				t.Fatalf("HTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
			low := strings.ToLower(got)
			for _, bad := range append(tt.mustNot, "<script", "onerror", "onclick", "onload") {
				if strings.Contains(low, strings.ToLower(bad)) {
					t.Fatalf("HTML(%q) = %q still contains %q", tt.in, got, bad)
				}
			}
			for _, keep := range tt.mustContain {
				if !strings.Contains(got, keep) {
					t.Fatalf("HTML(%q) = %q lost %q", tt.in, got, keep)
				}
			}
		})
	}
}
