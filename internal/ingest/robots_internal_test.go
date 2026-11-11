package ingest

import (
	"testing"
	"time"
)

// TestRobots_GroupsWildcardsAndCrawlDelay: our group wins over "*", the
// longest rule wins (allow on a tie), "*" and "$" match as RFC 9309 says,
// an empty Disallow allows, Crawl-delay is read and capped; the Drupal
// robots.txt of the pilot sites (2026-11-11) allows the club pages.
func TestRobots_GroupsWildcardsAndCrawlDelay(t *testing.T) {
	t.Parallel()
	r := parseRobots([]byte(`# comment
User-agent: *
Disallow: /private/
Allow: /private/open
Disallow: /*.pdf$
Crawl-delay: 10

User-agent: OtherBot
Disallow: /
`), robotsAgent)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/", true}, {"/tr/etkinlikler", true}, {"/private/x", false}, {"/private/open", true},
		{"/private/open/deeper", true}, {"/files/a.pdf", false}, {"/files/a.pdf?x=1", true},
	} {
		if got := r.allowed(tc.path); got != tc.want {
			t.Errorf("allowed(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if r.crawlDelay != 10*time.Second {
		t.Errorf("crawl delay = %s", r.crawlDelay)
	}

	own := parseRobots([]byte("User-agent: *\nDisallow: /\n\nUser-agent: KoroBot\nUser-agent: Another\nDisallow: /admin\nCrawl-delay: 99999\n"), robotsAgent)
	if !own.allowed("/kulupler") || own.allowed("/admin/x") || own.crawlDelay != maxCrawlDelay {
		t.Errorf("own group: kulupler %v admin %v delay %s", own.allowed("/kulupler"), own.allowed("/admin/x"), own.crawlDelay)
	}
	if all := parseRobots([]byte("User-agent: *\nDisallow:\n"), robotsAgent); !all.allowed("/anything") {
		t.Error("an empty Disallow refused a path")
	}
	if none := parseRobots(nil, robotsAgent); !none.allowed("/x") {
		t.Error("no robots.txt refused a path")
	}
	drupal := parseRobots([]byte("User-agent: *\nAllow: /core/*.css$\nDisallow: /core/\nDisallow: /admin/\nDisallow: /search/\nDisallow: /user/login\n"), robotsAgent)
	for _, p := range []string{"/kampus/kampus-yasami/ogrenci-kulupleri", "/kulupler?c=5", "/tr/ogrenci-topluluklari"} {
		if !drupal.allowed(p) {
			t.Errorf("drupal robots refused %s", p)
		}
	}
	if drupal.allowed("/admin/config") || !drupal.allowed("/core/a.css") || drupal.allowed("/core/a.js") {
		t.Error("drupal rules misread")
	}
}
