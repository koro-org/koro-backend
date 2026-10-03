// Package page implements keyset pagination with opaque cursors (05 §10.3,
// 04 §1 "Sayfalama"). Offset pagination and total counts are not offered.
//
// A cursor is base64url(JSON) of {v: format version, s: sort key name,
// k: sort values of the last row, f: filter fingerprint}. It is not signed
// and grants nothing: every page re-applies authz.Can and ScopeOrg. The
// fingerprint stops a cursor from being replayed with different filters.
// Anything that does not match exactly is 422 page.invalid_cursor.
//
// Flow: the handler calls Decode → the service asks the store for Limit+1
// rows → Cut returns (items, more) → if more, the handler encodes the last
// item with Next.
//
// The package imports only the standard library and platform/problem (the
// leaf error model) for ErrInvalidCursor.
package page

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// Limits (04 §1: limit 1–100, default 25).
const (
	DefaultLimit = 25
	MaxLimit     = 100
	MaxCursorLen = 512
	version      = 1
)

// ErrInvalidCursor is returned for malformed, foreign or mismatched cursors.
var ErrInvalidCursor = problem.New(problem.Invalid, "page.invalid_cursor", "Invalid cursor")

var b64 = base64.RawURLEncoding.Strict()

// Params is embedded in list operation inputs; huma applies the tags
// (limit 1–100, default 25; cursor ≤ 512 characters).
type Params struct {
	Limit  int    `query:"limit" minimum:"1" maximum:"100" default:"25" doc:"Page size"`
	Cursor string `query:"cursor" maxLength:"512" doc:"Opaque cursor from next_cursor"`
}

// Cursor is the decoded position after the last row of a page.
type Cursor struct {
	V    int      `json:"v"`
	Sort string   `json:"s"`
	Vals []string `json:"k"`
	F    string   `json:"f,omitempty"`
}

// Next encodes a cursor for sort key sort, filter fingerprint filterFP and
// the sort values of the last returned row (use TimeVal / UUIDVal).
func Next(sort, filterFP string, vals ...string) string {
	b, err := json.Marshal(Cursor{V: version, Sort: sort, Vals: vals, F: filterFP})
	if err != nil { // a struct of strings always marshals
		panic("page: marshal cursor: " + err.Error())
	}
	return b64.EncodeToString(b)
}

// Decode validates s against the expected sort key, filter fingerprint and
// number of values. An empty s is the first page: (nil, nil).
func Decode(s, sort, filterFP string, nvals int) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	if len(s) > MaxCursorLen {
		return nil, ErrInvalidCursor
	}
	raw, err := b64.DecodeString(s)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var c Cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, ErrInvalidCursor
	}
	// Only the exact bytes Next produces are accepted: no unknown, duplicate
	// or differently cased keys, no whitespace or trailing data.
	if canon, err := json.Marshal(c); err != nil || !bytes.Equal(canon, raw) {
		return nil, ErrInvalidCursor
	}
	if c.V != version || c.Sort != sort || c.F != filterFP || len(c.Vals) != nvals {
		return nil, ErrInvalidCursor
	}
	return &c, nil
}

// TimeVal formats a timestamp sort value (UTC, RFC 3339 with nanoseconds).
func TimeVal(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// UUIDVal formats an id sort value.
func UUIDVal(id uuid.UUID) string { return id.String() }

// Time parses value i as a timestamp written by TimeVal.
func (c *Cursor) Time(i int) (time.Time, error) {
	if i < 0 || i >= len(c.Vals) {
		return time.Time{}, ErrInvalidCursor
	}
	t, err := time.Parse(time.RFC3339Nano, c.Vals[i])
	if err != nil || t.Location() != time.UTC {
		return time.Time{}, ErrInvalidCursor
	}
	return t, nil
}

// UUID parses value i as an id written by UUIDVal.
func (c *Cursor) UUID(i int) (uuid.UUID, error) {
	if i < 0 || i >= len(c.Vals) {
		return uuid.UUID{}, ErrInvalidCursor
	}
	id, err := uuid.Parse(c.Vals[i])
	if err != nil || id.String() != c.Vals[i] {
		return uuid.UUID{}, ErrInvalidCursor
	}
	return id, nil
}

// Fingerprint is a short, non-secret digest of the filter values of a list
// request. Parts are length-prefixed, so ("ab", "c") ≠ ("a", "bc").
func Fingerprint(parts ...string) string {
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, p := range parts {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(p)))])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// ClampLimit returns the page size to use: DefaultLimit for n <= 0, at most
// MaxLimit.
func ClampLimit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}

// Cut trims rows fetched with limit+1 to limit and reports whether another
// page exists.
func Cut[T any](rows []T, limit int) (items []T, more bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}
