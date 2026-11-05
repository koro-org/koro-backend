package notify

import (
	"regexp"
	"strings"
	"testing"
)

// TestSuppressionIDPattern (BE-175): the documented syntax of the
// suppression_id path parameter takes exactly what parseSuppressionID
// takes.
func TestSuppressionIDPattern(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(suppressionIDPattern)
	ok := SuppressionID([]byte(strings.Repeat("\xab", 32)))
	for _, s := range []string{ok, strings.Repeat("0", 64), "", strings.Repeat("a", 63), strings.Repeat("a", 66),
		strings.ToUpper(ok), strings.Repeat("g", 64), "019a0000-0000-7000-8000-000000000001"} {
		_, err := parseSuppressionID(s)
		if re.MatchString(s) != (err == nil) {
			t.Errorf("%q: pattern %v, parse error %v", s, re.MatchString(s), err)
		}
	}
}
