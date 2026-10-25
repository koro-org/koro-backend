package textx

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
)

// TrimSlug cuts a slug to n bytes without a trailing hyphen.
func TrimSlug(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimRight(s[:n], "-")
}

// FreeSlug finds a free slug for a new row whose slug is unique: it calls
// insert with base, then base-2 … base-<attempts>, then base-<8 random hex
// characters>, each cut to maxLen bytes (TrimSlug before the suffix), until
// insert reports the slug free (true). insert writes the row with the slug
// and returns false when the slug is taken. FreeSlug returns false when
// every candidate was taken, and the first error of insert.
func FreeSlug(base string, maxLen, attempts int, insert func(slug string) (bool, error)) (bool, error) {
	for i := 1; i <= attempts+1; i++ {
		var suffix string
		switch {
		case i == 1:
		case i <= attempts:
			suffix = "-" + strconv.Itoa(i)
		default:
			b := make([]byte, 4)
			_, _ = rand.Read(b)
			suffix = "-" + hex.EncodeToString(b)
		}
		ok, err := insert(TrimSlug(base, maxLen-len(suffix)) + suffix)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}
