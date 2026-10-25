package outreach

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"uuid"

	"koro/internal/platform/problem"
)

// Audience key prefixes (02 §9.1): the keys count_segment, the feed and
// the dispatch match. 'all' is never produced here: a segment always
// narrows (02 §13.1: an empty definition is refused).
const (
	keyUni  = "uni:"
	keyDept = "dept:"
	keyYear = "year:"
	keyClub = "club:"
)

// Limits of a definition. Each dimension holds at most maxPerDimension
// values (a company may OR many universities or departments); the students
// module accepts up to 500 keys, a definition produces at most 308.
const (
	maxPerDimension = 100
	minYear         = 1
	maxYear         = 8
)

// Definition is a targeting (02 §13.1 segments.definition): OR inside a
// dimension, AND across the dimensions present. The dimensions are the
// university (universities.id), the department (departments.id), the class
// year (1–8) and the club (the club's org ID). A student's profile carries
// at most one key per dimension except clubs, so a definition matches a
// student when every present dimension has one of the student's keys.
//
// Talent values (points, bands, scores) are no dimension and never will be
// (01 §0-7): nothing in a definition reads them.
type Definition struct {
	Uni  []uuid.UUID `json:"uni,omitempty"`
	Dept []uuid.UUID `json:"dept,omitempty"`
	Year []int16     `json:"year,omitempty"`
	Club []uuid.UUID `json:"club,omitempty"`
}

// normalize validates d and returns it with every dimension sorted and
// without duplicates. loc is the field location of the definition
// ("body.definition" on the HTTP path).
func (d Definition) normalize(loc string) (Definition, error) {
	at := func(dim string) string {
		if loc == "" {
			return dim
		}
		return loc + "." + dim
	}
	ids := func(dim string, in []uuid.UUID) ([]uuid.UUID, error) {
		if len(in) > maxPerDimension {
			return nil, problem.Field(at(dim), "at most "+strconv.Itoa(maxPerDimension)+" values")
		}
		out := slices.Clone(in)
		slices.SortFunc(out, uuid.UUID.Compare)
		out = slices.Compact(out)
		if slices.Contains(out, uuid.Nil()) {
			return nil, problem.Field(at(dim), "must not contain the nil UUID")
		}
		if len(out) == 0 {
			return nil, nil
		}
		return out, nil
	}
	var (
		n   Definition
		err error
	)
	if n.Uni, err = ids("uni", d.Uni); err != nil {
		return Definition{}, err
	}
	if n.Dept, err = ids("dept", d.Dept); err != nil {
		return Definition{}, err
	}
	if n.Club, err = ids("club", d.Club); err != nil {
		return Definition{}, err
	}
	if len(d.Year) > maxPerDimension {
		return Definition{}, problem.Field(at("year"), "at most "+strconv.Itoa(maxPerDimension)+" values")
	}
	for _, y := range d.Year {
		if y < minYear || y > maxYear {
			return Definition{}, problem.Field(at("year"), "class years are 1-8")
		}
	}
	if len(d.Year) > 0 {
		n.Year = slices.Compact(slices.Sorted(slices.Values(d.Year)))
	}
	if n.DimCount() == 0 {
		return Definition{}, problem.Field(loc, "at least one of uni, dept, year, club is required")
	}
	return n, nil
}

// DimCount is the number of dimensions present (1–4 for a valid
// definition; segments_audience_dim_count_chk).
func (d Definition) DimCount() int16 {
	var n int16
	for _, present := range []bool{len(d.Uni) > 0, len(d.Dept) > 0, len(d.Year) > 0, len(d.Club) > 0} {
		if present {
			n++
		}
	}
	return n
}

// Keys returns the audience keys of d, sorted: "uni:<id>", "dept:<id>",
// "year:<n>", "club:<id>" (02 §9.1).
func (d Definition) Keys() []string {
	keys := make([]string, 0, len(d.Uni)+len(d.Dept)+len(d.Year)+len(d.Club))
	for _, id := range d.Uni {
		keys = append(keys, keyUni+id.String())
	}
	for _, id := range d.Dept {
		keys = append(keys, keyDept+id.String())
	}
	for _, y := range d.Year {
		keys = append(keys, keyYear+strconv.Itoa(int(y)))
	}
	for _, id := range d.Club {
		keys = append(keys, keyClub+id.String())
	}
	slices.Sort(keys)
	return keys
}

// clone returns a deep copy of d.
func (d Definition) clone() Definition {
	return Definition{Uni: slices.Clone(d.Uni), Dept: slices.Clone(d.Dept), Year: slices.Clone(d.Year), Club: slices.Clone(d.Club)}
}

// Value implements driver.Valuer (segments.definition jsonb).
func (d Definition) Value() (driver.Value, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, fmt.Errorf("outreach: definition: %w", err)
	}
	return string(b), nil
}

// Scan implements sql.Scanner (jsonb arrives as text or bytes).
func (d *Definition) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("outreach: definition: unexpected %T", src)
	}
	var out Definition
	if err := json.Unmarshal(b, &out); err != nil {
		return fmt.Errorf("outreach: definition: %w", err)
	}
	*d = out
	return nil
}
