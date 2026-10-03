package migratekit

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// FrozenFile is the frozen floor manifest next to the migrations (BE-001
// R2): written at the pilot deployment with `koro migrate freeze <version>`.
const FrozenFile = ".frozen"

var (
	nameRe        = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)
	beginRe       = regexp.MustCompile(`^(BEGIN|START\s+TRANSACTION)\b`)
	commitRe      = regexp.MustCompile(`^(COMMIT|END)\b`)
	concurrentRe  = regexp.MustCompile(`\bCONCURRENTLY\b`)
	doRe          = regexp.MustCompile(`^DO\b`)
	truncateRe    = regexp.MustCompile(`^TRUNCATE\b`)
	destructiveRe = regexp.MustCompile(`\b(DROP\s+(TABLE|COLUMN|SCHEMA)|DELETE\s+FROM|RENAME\s+(TO|COLUMN))\b`)
)

// allowDestructive marks an up migration whose destructive statement is
// intended: "-- koro:allow-destructive <reason, ticket>" (05 §7.2).
const allowDestructive = "koro:allow-destructive"

// migrationFile is one parsed file name.
type migrationFile struct {
	name      string
	version   uint
	desc      string
	direction string // up | down
}

// scan lists the migration files of fsys. Go files and the frozen manifest
// are ignored; any other file is an error.
func scan(fsys fs.FS) ([]migrationFile, []error) {
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, []error{fmt.Errorf("migratekit: read migrations: %w", err)}
	}
	var (
		files []migrationFile
		errs  []error
	)
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || strings.HasSuffix(name, ".go") || name == FrozenFile {
			continue
		}
		m := nameRe.FindStringSubmatch(name)
		if m == nil {
			errs = append(errs, fmt.Errorf("%s: invalid name: want NNNNNN_<snake_case>.{up,down}.sql", name))
			continue
		}
		v, _ := strconv.ParseUint(m[1], 10, 32)
		if v == 0 {
			errs = append(errs, fmt.Errorf("%s: invalid name: numbers start at 000001", name))
			continue
		}
		files = append(files, migrationFile{name: name, version: uint(v), desc: m[2], direction: m[3]})
	}
	return files, errs
}

// Lint checks the migration rules of 05 §7 and BE-001 R2 on fsys (the
// directory holding the *.sql files and, once frozen, .frozen):
//
//   - names are NNNNNN_<snake_case>.{up,down}.sql; one name per number; every
//     number has both an up and a down file;
//   - a file with more than one statement is wrapped in BEGIN … COMMIT;
//   - a CONCURRENTLY file holds exactly one statement and no BEGIN;
//   - a destructive statement in an up file (DROP TABLE/COLUMN/SCHEMA,
//     TRUNCATE, DELETE FROM, RENAME TO/COLUMN; DO blocks included, function
//     bodies excluded) needs "-- koro:allow-destructive <reason>";
//   - with .frozen: files numbered <= the floor are unchanged (SHA-256), none
//     was added or removed there; new files are above the floor.
//
// Gaps between numbers are allowed: numbers are pre-allocated per ticket and
// merge in any order before the pilot (R2).
func Lint(fsys fs.FS) []error {
	files, errs := scan(fsys)

	type pair struct {
		desc     string
		up, down bool
	}
	pairs := map[uint]*pair{}
	for _, f := range files {
		p := pairs[f.version]
		switch {
		case p == nil:
			p = &pair{desc: f.desc}
			pairs[f.version] = p
		case p.desc != f.desc:
			errs = append(errs, fmt.Errorf("%06d: number used by two migrations (%s, %s)", f.version, p.desc, f.desc))
		}
		if f.direction == "up" {
			p.up = true
		} else {
			p.down = true
		}
		src, err := fs.ReadFile(fsys, f.name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.name, err))
			continue
		}
		errs = append(errs, lintFile(f, string(src))...)
	}
	for _, v := range slices.Sorted(maps.Keys(pairs)) {
		if p := pairs[v]; !p.up || !p.down {
			errs = append(errs, fmt.Errorf("%06d_%s: up/down pair incomplete", v, p.desc))
		}
	}
	errs = append(errs, lintFrozen(fsys, files)...)
	return errs
}

// lintFile checks the statements of one file.
func lintFile(f migrationFile, src string) []error {
	stmts, err := splitSQL(src)
	if err != nil {
		return []error{fmt.Errorf("%s: %w", f.name, err)}
	}
	var errs []error
	concurrent := false
	for _, s := range stmts {
		concurrent = concurrent || concurrentRe.MatchString(s.bare)
	}
	switch {
	case concurrent:
		if len(stmts) != 1 || beginRe.MatchString(stmts[0].bare) {
			errs = append(errs, fmt.Errorf("%s: a CONCURRENTLY migration must be a single statement without BEGIN", f.name))
		}
	case len(stmts) > 1:
		if !beginRe.MatchString(stmts[0].bare) || !commitRe.MatchString(stmts[len(stmts)-1].bare) {
			errs = append(errs, fmt.Errorf("%s: a multi-statement migration must be wrapped in BEGIN … COMMIT", f.name))
		}
	}
	if f.direction == "up" && !strings.Contains(src, allowDestructive) {
		for _, s := range stmts {
			text := s.bare
			if doRe.MatchString(s.bare) {
				text = s.code // a DO body runs during the migration
			}
			if truncateRe.MatchString(s.bare) || destructiveRe.MatchString(text) {
				errs = append(errs, fmt.Errorf("%s: destructive statement needs '-- %s <reason, ticket>'", f.name, allowDestructive))
				break
			}
		}
	}
	return errs
}

// statement is one SQL statement without comments, upper-cased: code keeps
// string and dollar-quoted contents, bare replaces them with empty quotes.
type statement struct{ code, bare string }

// splitSQL splits src on top-level semicolons, honouring -- and /* */
// comments, '…' and E'…' strings, "…" identifiers and $tag$…$tag$ bodies.
func splitSQL(src string) ([]statement, error) {
	var (
		out        []statement
		code, bare strings.Builder
	)
	flush := func() {
		c, b := strings.TrimSpace(code.String()), strings.TrimSpace(bare.String())
		if b != "" {
			out = append(out, statement{code: strings.ToUpper(c), bare: strings.ToUpper(b)})
		}
		code.Reset()
		bare.Reset()
	}
	both := func(s string) { code.WriteString(s); bare.WriteString(s) }
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case strings.HasPrefix(src[i:], "--"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				i = len(src)
			} else {
				i += j
			}
		case strings.HasPrefix(src[i:], "/*"):
			j, err := blockCommentEnd(src, i)
			if err != nil {
				return nil, err
			}
			both(" ")
			i = j
		case c == '\'' || ((c == 'E' || c == 'e') && i+1 < len(src) && src[i+1] == '\'' && !identByte(src, i-1)):
			start := i
			if c != '\'' {
				i++
			}
			j, err := quotedEnd(src, i, c != '\'')
			if err != nil {
				return nil, err
			}
			code.WriteString(src[start:j])
			bare.WriteString("''")
			i = j
		case c == '"':
			j := strings.IndexByte(src[i+1:], '"')
			if j < 0 {
				return nil, errors.New(`unterminated quoted identifier`)
			}
			both(src[i : i+j+2])
			i += j + 2
		case c == '$':
			tag, ok := dollarTag(src, i)
			if !ok {
				both("$")
				i++
				continue
			}
			j := strings.Index(src[i+len(tag):], tag)
			if j < 0 {
				return nil, fmt.Errorf("unterminated dollar-quoted body %s", tag)
			}
			end := i + len(tag) + j + len(tag)
			code.WriteString(src[i:end])
			bare.WriteString("$$$$")
			i = end
		case c == ';':
			flush()
			i++
		default:
			both(string(c))
			i++
		}
	}
	flush()
	return out, nil
}

func identByte(s string, i int) bool {
	if i < 0 {
		return false
	}
	c := s[i]
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// blockCommentEnd returns the index after the (possibly nested) comment
// starting at i.
func blockCommentEnd(s string, i int) (int, error) {
	depth := 0
	for i < len(s) {
		switch {
		case strings.HasPrefix(s[i:], "/*"):
			depth++
			i += 2
		case strings.HasPrefix(s[i:], "*/"):
			depth--
			i += 2
			if depth == 0 {
				return i, nil
			}
		default:
			i++
		}
	}
	return 0, errors.New("unterminated block comment")
}

// quotedEnd returns the index after the string literal whose opening quote
// is at i; ” is an escaped quote, and \' too in E” strings.
func quotedEnd(s string, i int, escapes bool) (int, error) {
	for j := i + 1; j < len(s); j++ {
		switch {
		case escapes && s[j] == '\\':
			j++
		case s[j] == '\'' && j+1 < len(s) && s[j+1] == '\'':
			j++
		case s[j] == '\'':
			return j + 1, nil
		}
	}
	return 0, errors.New("unterminated string literal")
}

// dollarTag returns the $tag$ opening at i ($1 parameters are not tags).
func dollarTag(s string, i int) (string, bool) {
	j := i + 1
	for j < len(s) && (s[j] == '_' || s[j] >= 'a' && s[j] <= 'z' || s[j] >= 'A' && s[j] <= 'Z' || j > i+1 && s[j] >= '0' && s[j] <= '9') {
		j++
	}
	if j < len(s) && s[j] == '$' && !identByte(s, i-1) {
		return s[i : j+1], true
	}
	return "", false
}

// frozen is a parsed .frozen manifest.
type frozen struct {
	floor uint
	sums  map[string]string // file name → hex SHA-256
}

func parseFrozen(b []byte) (frozen, error) {
	f := frozen{sums: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(b))
	seenFloor := false
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[0] == "floor" && !seenFloor:
			v, err := strconv.ParseUint(fields[1], 10, 32)
			if err != nil || v == 0 {
				return f, fmt.Errorf("%s line %d: invalid floor %q", FrozenFile, n, fields[1])
			}
			f.floor, seenFloor = uint(v), true
		case len(fields) == 2 && len(fields[0]) == sha256.Size*2:
			if _, err := hex.DecodeString(fields[0]); err != nil {
				return f, fmt.Errorf("%s line %d: invalid checksum", FrozenFile, n)
			}
			f.sums[fields[1]] = fields[0]
		default:
			return f, fmt.Errorf("%s line %d: want 'floor N' or '<sha256> <file>'", FrozenFile, n)
		}
	}
	if !seenFloor {
		return f, fmt.Errorf("%s: missing 'floor N' line", FrozenFile)
	}
	if len(f.sums) == 0 {
		return f, fmt.Errorf("%s: no checksums: regenerate it with `koro migrate freeze %d`", FrozenFile, f.floor)
	}
	return f, nil
}

func lintFrozen(fsys fs.FS, files []migrationFile) []error {
	b, err := fs.ReadFile(fsys, FrozenFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // before the pilot: files may still be fixed in place (R2)
	}
	if err != nil {
		return []error{fmt.Errorf("%s: %w", FrozenFile, err)}
	}
	fr, err := parseFrozen(b)
	if err != nil {
		return []error{err}
	}
	var errs []error
	present := map[string]bool{}
	for _, f := range files {
		present[f.name] = true
		if f.version > fr.floor {
			continue
		}
		want, listed := fr.sums[f.name]
		if !listed {
			errs = append(errs, fmt.Errorf("%s: added at or below the frozen floor %06d: new migrations take a number above it", f.name, fr.floor))
			continue
		}
		src, err := fs.ReadFile(fsys, f.name)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.name, err))
			continue
		}
		if got := sha256.Sum256(src); hex.EncodeToString(got[:]) != want {
			errs = append(errs, fmt.Errorf("%s: changed after it was frozen (floor %06d): write a new migration instead", f.name, fr.floor))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(fr.sums)) {
		if !present[name] {
			errs = append(errs, fmt.Errorf("%s: frozen migration was removed or renamed", name))
		}
	}
	return errs
}

// Freeze returns the .frozen manifest that freezes every migration numbered
// <= floor (the version deployed to the pilot).
func Freeze(fsys fs.FS, floor uint) ([]byte, error) {
	files, errs := scan(fsys)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	latest := uint(0)
	for _, f := range files {
		latest = max(latest, f.version)
	}
	if floor == 0 || floor > latest {
		return nil, fmt.Errorf("migratekit: freeze floor must be between 1 and %d", latest)
	}
	slices.SortFunc(files, func(a, b migrationFile) int { return strings.Compare(a.name, b.name) })
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# Frozen migration floor (BE-001 R2, 05 §7.1): files numbered <= floor were\n")
	fmt.Fprintf(&buf, "# deployed and never change; new migrations take max+1. Generated by\n")
	fmt.Fprintf(&buf, "# `koro migrate freeze %d`; checked by migratekit.Lint (make test).\n", floor)
	fmt.Fprintf(&buf, "floor %06d\n", floor)
	for _, f := range files {
		if f.version > floor {
			continue
		}
		src, err := fs.ReadFile(fsys, f.name)
		if err != nil {
			return nil, fmt.Errorf("migratekit: %s: %w", f.name, err)
		}
		sum := sha256.Sum256(src)
		fmt.Fprintf(&buf, "%s  %s\n", hex.EncodeToString(sum[:]), f.name)
	}
	return buf.Bytes(), nil
}
