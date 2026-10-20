package talent_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"koro/internal/talent"
)

// TestBand_MarshalJSONPanics (01 §4.11, §0-7): a Band cannot be serialised
// by accident: JSON (alone, in a struct, as a map value), text and map
// keys panic; fmt and slog print it redacted.
func TestBand_MarshalJSONPanics(t *testing.T) {
	t.Parallel()
	b := talent.Band(3)
	for name, v := range map[string]any{
		"band":       b,
		"in struct":  struct{ Band talent.Band }{b},
		"pointer":    &b,
		"map value":  map[string]talent.Band{"x": b},
		"map key":    map[talent.Band]int{b: 1},
		"in a slice": []talent.Band{b},
	} {
		func() {
			defer func() {
				r := recover()
				if r == nil || !strings.Contains(fmt.Sprint(r), "never be serialised") {
					t.Errorf("%s: json.Marshal did not panic (%v)", name, r)
				}
			}()
			_, _ = json.Marshal(v)
		}()
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("MarshalText did not panic")
			}
		}()
		_, _ = b.MarshalText()
	}()
	for _, s := range []string{fmt.Sprint(b), fmt.Sprintf("%v %s %#v", b, b, b)} {
		if strings.Contains(s, "3") {
			t.Errorf("fmt printed the band: %q", s)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "band", b)
	if strings.Contains(buf.String(), `"band":3`) || !strings.Contains(buf.String(), "redacted") {
		t.Errorf("slog printed the band: %s", buf.String())
	}
}
