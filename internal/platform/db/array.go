package db

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"uuid"

	"github.com/jackc/pgx/v5/pgtype"
)

// TextArray is a text[] column or parameter (02 §0.4: audience keys, flag
// sets, field lists). GORM expands a plain []string parameter into a row
// expression "($1,$2)" and cannot store it in a column; TextArray binds as
// one array value. NULL scans as an empty array.
type TextArray []string

var arrayEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// Value encodes a as a PostgreSQL array literal ({"a","b c"}).
func (a TextArray) Value() (driver.Value, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(arrayEscaper.Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan decodes a text[] value.
func (a *TextArray) Scan(src any) error {
	var out []string
	if err := scanArray(pgtype.TextArrayOID, src, &out); err != nil {
		return err
	}
	*a = out
	return nil
}

// UUIDArray is a uuid[] column or parameter; use it with "= ANY(?)" and
// array operators. For "IN ?" a plain []uuid.UUID is fine.
type UUIDArray []uuid.UUID

// Value encodes a as a PostgreSQL array literal.
func (a UUIDArray) Value() (driver.Value, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, u := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(u.String())
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan decodes a uuid[] value.
func (a *UUIDArray) Scan(src any) error {
	var raw [][16]byte
	if err := scanArray(pgtype.UUIDArrayOID, src, &raw); err != nil {
		return err
	}
	out := make(UUIDArray, len(raw))
	for i, r := range raw {
		out[i] = uuid.UUID(r)
	}
	*a = out
	return nil
}

// scanArray decodes the text form of an array (database/sql hands arrays
// over as strings) with pgx's codecs. NULL becomes an empty slice.
func scanArray(oid uint32, src any, dst any) error {
	var text []byte
	switch v := src.(type) {
	case nil:
		text = []byte("{}")
	case string:
		text = []byte(v)
	case []byte:
		text = v
	default:
		return fmt.Errorf("db: cannot scan %T into an array", src)
	}
	if err := pgtype.NewMap().Scan(oid, pgtype.TextFormatCode, text, dst); err != nil {
		return fmt.Errorf("db: scan array: %w", err)
	}
	return nil
}
