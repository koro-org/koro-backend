package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
	"uuid"

	"koro/internal/platform/civil"
)

// HashSize is the size of prev_hash, row_hash and root_hash.
const HashSize = sha256.Size

// zeroHash is the prev_hash of the first chained row (02 §2.1).
var zeroHash = make([]byte, HashSize)

// RowHash is the one hash function of the chain (02 §2.1): SHA-256 of
// prev ‖ CanonicalJSON(r). The chain job and every verification use it.
func RowHash(prev []byte, r Record) ([]byte, error) {
	c, err := CanonicalJSON(r)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write(prev)
	h.Write(c)
	return h.Sum(nil), nil
}

// CanonicalJSON is the row without its chain columns as canonical JSON:
// keys in alphabetical order, every column present (NULL as null), times
// in RFC 3339 with nanoseconds in UTC, UUIDs in lower-case text, the IP in
// netip text, metadata re-encoded with sorted keys and its numbers as
// written, no HTML escaping and no insignificant whitespace.
func CanonicalJSON(r Record) ([]byte, error) {
	meta, err := canonicalMetadata(r.Metadata)
	if err != nil {
		return nil, err
	}
	m := map[string]any{
		"action":        r.Action,
		"actor_kind":    r.ActorKind,
		"actor_org_id":  uuidOrNil(r.ActorOrgID),
		"actor_user_id": uuidOrNil(r.ActorUserID),
		"id":            r.ID.String(),
		"ip":            strOrNil(r.IP),
		"metadata":      meta,
		"occurred_at":   r.OccurredAt.UTC().Format(time.RFC3339Nano),
		"org_id":        uuidOrNil(r.OrgID),
		"outcome":       string(r.Outcome),
		"reason":        strOrNil(r.Reason),
		"request_id":    strOrNil(r.RequestID),
		"resource_id":   uuidOrNil(r.ResourceID),
		"resource_type": strOrNil(r.ResourceType),
		"seq":           r.Seq,
		"system_job":    strOrNil(r.SystemJob),
		"user_agent":    strOrNil(r.UserAgent),
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("audit: canonical json: %w", err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// canonicalMetadata decodes a metadata object keeping numbers as written;
// encoding/json writes the map keys sorted.
func canonicalMetadata(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("audit: canonical metadata: %w", err)
	}
	return v, nil
}

func uuidOrNil(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// rootMessage is the signed content of a daily root (v1).
func rootMessage(day civil.Date, lastChainSeq int64, rootHash []byte) []byte {
	return []byte("koro.audit.root.v1|" + day.String() + "|" + strconv.FormatInt(lastChainSeq, 10) + "|" + hex.EncodeToString(rootHash))
}
