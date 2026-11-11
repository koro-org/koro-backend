package ingest

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"time"
)

// robots.txt (RFC 9309): the group of our product token ("korobot", case
// insensitive) or else "*"; the longest matching rule wins, allow on a tie;
// "*" matches any sequence and a final "$" anchors the end; an empty
// Disallow allows. Crawl-delay (not in the RFC, honoured as the research
// asks: ODTÜ has one) is read from the chosen group, capped at maxCrawlDelay.

// maxCrawlDelay caps a Crawl-delay: a host asking more is still fetched,
// at most once per this period.
const maxCrawlDelay = 10 * time.Minute

// robotsRules is the chosen group of a robots.txt.
type robotsRules struct {
	rules      []robotsRule
	crawlDelay time.Duration
}

type robotsRule struct {
	allow   bool
	pattern string
}

// parseRobots reads a robots.txt for the product token agent (lower case).
func parseRobots(body []byte, agent string) robotsRules {
	type group struct {
		agents []string
		rules  []robotsRule
		delay  time.Duration
	}
	var groups []*group
	var cur *group
	lastWasAgent := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 512<<10)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		switch key {
		case "user-agent":
			if cur == nil || !lastWasAgent {
				cur = &group{}
				groups = append(groups, cur)
			}
			cur.agents = append(cur.agents, strings.ToLower(val))
			lastWasAgent = true
			continue
		case "allow", "disallow":
			if cur != nil {
				cur.rules = append(cur.rules, robotsRule{allow: key == "allow", pattern: val})
			}
		case "crawl-delay":
			if cur != nil {
				if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
					cur.delay = min(time.Duration(f*float64(time.Second)), maxCrawlDelay)
				}
			}
		}
		lastWasAgent = false
	}
	pick := func(match func(string) bool) *robotsRules {
		var out *robotsRules
		for _, g := range groups {
			for _, a := range g.agents {
				if match(a) {
					if out == nil {
						out = &robotsRules{}
					}
					out.rules = append(out.rules, g.rules...)
					out.crawlDelay = max(out.crawlDelay, g.delay)
					break
				}
			}
		}
		return out
	}
	if r := pick(func(a string) bool { return a != "*" && strings.Contains(agent, a) }); r != nil {
		return *r
	}
	if r := pick(func(a string) bool { return a == "*" }); r != nil {
		return *r
	}
	return robotsRules{}
}

// allowed reports whether path (with its query) may be fetched.
func (r robotsRules) allowed(path string) bool {
	if path == "" {
		path = "/"
	}
	best, allow := -1, true
	for _, rule := range r.rules {
		if rule.pattern == "" {
			continue // an empty Disallow allows everything; an empty Allow says nothing
		}
		if robotsMatch(rule.pattern, path) {
			n := len(rule.pattern)
			if n > best || n == best && rule.allow {
				best, allow = n, rule.allow
			}
		}
	}
	return allow
}

// robotsMatch matches path against a robots pattern ("*" any run, final "$"
// end anchor), from the start of the path.
func robotsMatch(pattern, path string) bool {
	anchored := strings.HasSuffix(pattern, "$")
	if anchored {
		pattern = pattern[:len(pattern)-1]
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(path, parts[0]) {
		return false
	}
	pos := len(parts[0])
	for i := 1; i < len(parts); i++ {
		p := parts[i]
		if i == len(parts)-1 && anchored {
			return strings.HasSuffix(path[pos:], p)
		}
		j := strings.Index(path[pos:], p)
		if j < 0 {
			return false
		}
		pos += j + len(p)
	}
	return !anchored || pos == len(path)
}
