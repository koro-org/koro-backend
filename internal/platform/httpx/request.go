package httpx

import (
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/platform/problem"
)

// The request helpers below carry the 404/412/422/428 contract of the
// module handlers (04 §1.4, §1.10, §1.13). Every module uses them instead of
// a private copy (BE-111; internal/arch bans the copies).

// ETag is the strong entity tag of a row: its updated_at in nanoseconds,
// base 32, quoted (04 §1.13). A row's tag changes with every update.
func ETag(updatedAt time.Time) string {
	return `"` + strconv.FormatInt(updatedAt.UnixNano(), 32) + `"`
}

// CheckIfMatch applies If-Match (04 §1.13): a missing header or "*" →
// problem.ErrPreconditionRequired (428), no listed tag equal to current →
// problem.ErrPreconditionFailed (412).
func CheckIfMatch(header, current string) error {
	h := strings.TrimSpace(header)
	if h == "" || h == "*" {
		return problem.ErrPreconditionRequired
	}
	for tag := range strings.SplitSeq(h, ",") {
		if strings.TrimSpace(tag) == current {
			return nil
		}
	}
	return problem.ErrPreconditionFailed
}

// ParseID parses a path ID. A malformed ID becomes uuid.Nil(), which no row
// has: the lookup answers 404 like an unknown ID, so the path never tells a
// malformed ID from a missing one (04 §1.4).
func ParseID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil()
	}
	return id
}

// OptID parses an optional ID of a query or body field: "" → nil, a
// malformed ID → 422 validation_failed at loc ("must be a UUID").
func OptID(loc, s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, problem.Field(loc, "must be a UUID")
	}
	return &id, nil
}

// IDStr renders an optional ID for a DTO: nil → "".
func IDStr(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
