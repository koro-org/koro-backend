package seed

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"time"
	"uuid"
)

// idAnchor is the time part of every demo ID (UUIDv7 layout): a fixed
// instant plus the row's ordinal, so the IDs do not depend on when the
// seed runs (idempotency) and follow the generation order.
var idAnchor = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

// demoID is the deterministic ID of the demo row named key: the UUIDv7 bit
// layout with idAnchor + ord milliseconds as its time and SHA-256("koro-
// demo/" + key) as its random part. The same key and ordinal give the same
// ID on every run, which is what makes `ON CONFLICT DO NOTHING` idempotent.
func demoID(key string, ord int) uuid.UUID {
	var u uuid.UUID
	ms := uint64(idAnchor.UnixMilli()) + uint64(ord) //nolint:gosec // G115: the anchor and the ordinal are small positive numbers
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(u[:6], ts[2:])
	h := sha256.Sum256([]byte("koro-demo/" + key))
	copy(u[6:], h[:10])
	u[6] = 0x70 | (u[6] & 0x0f) // version 7
	u[8] = 0x80 | (u[8] & 0x3f) // RFC 9562 variant
	return u
}

// Row is one INSERT of the plan: a table and its column values in order.
type Row struct {
	Table string
	Cols  []string
	Vals  []any
}

// typed is a value with an explicit SQL cast ($n::<type>): jsonb and date
// values, which the QueryExecModeExec pool cannot infer from a Go string.
type typed struct {
	v   any
	typ string
}

// jsonb is a JSON value for a jsonb column.
func jsonb(v any) typed {
	b, err := json.Marshal(v)
	if err != nil {
		panic("seed: unencodable JSON: " + err.Error()) // programmer error: plan values are plain maps and slices
	}
	return typed{string(b), "jsonb"}
}

// rawJSON is already encoded JSON for a jsonb column.
func rawJSON(b []byte) typed { return typed{string(b), "jsonb"} }

// date is a date-only value ("YYYY-MM-DD").
func date(t time.Time) typed { return typed{t.Format(time.DateOnly), "date"} }

// add appends a row; kv alternates column names and values.
func (p *Plan) add(table string, kv ...any) {
	r := Row{Table: table}
	for i := 0; i+1 < len(kv); i += 2 {
		col, ok := kv[i].(string)
		if !ok {
			panic("seed: column name is not a string") // programmer error in the builder
		}
		r.Cols = append(r.Cols, col)
		r.Vals = append(r.Vals, kv[i+1])
	}
	p.Rows = append(p.Rows, r)
}

// Strings returns every text value of the plan (TestSeedDemo_NoRealPII
// scans them): plain strings, string slices and the JSON of jsonb values.
func (p *Plan) Strings() []string {
	var out []string
	for _, r := range p.Rows {
		for _, v := range r.Vals {
			switch x := v.(type) {
			case string:
				out = append(out, x)
			case *string:
				if x != nil {
					out = append(out, *x)
				}
			case []string:
				out = append(out, x...)
			case typed:
				if s, ok := x.v.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// Counts returns the number of planned rows per table.
func (p *Plan) Counts() map[string]int {
	out := map[string]int{}
	for _, r := range p.Rows {
		out[r.Table]++
	}
	return out
}
