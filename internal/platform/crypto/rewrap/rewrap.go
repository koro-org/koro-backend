// Package rewrap is the crypto.rewrap job (BE-055, T-82): it moves every
// envelope-encrypted column value (`*_enc bytea`, 02 §0.4) to the active
// key-encryption key — from the local KEK to OpenBao Transit, and after a
// Transit key rotation to the newest version.
//
// Properties:
//
//   - Idempotent and resumable. A run selects only values whose header
//     names another key than the active one (SQL on the header bytes), so
//     progress needs no cursor table: an interrupted run (deploy, crash,
//     time budget) simply leaves the rest for the next one, and a finished
//     migration makes every run a cheap no-op.
//   - Never writes clear text, never loses a value. Each value is opened and
//     sealed again in memory (crypto.Envelope.Rewrap); the new ciphertext
//     replaces the old one with a compare-and-swap UPDATE (… WHERE key = ?
//     AND col = old), one short transaction per batch, no remote call inside
//     a transaction. A row the application changed meanwhile is skipped and
//     picked up by the next run.
//   - Fail closed. If the key service is unavailable the run stops with an
//     error before writing anything for that value; a value that cannot be
//     opened (corrupt, unknown key) is counted, logged with its row key and
//     an alarm flag, and left untouched.
//
// Columns are declared by the module that owns the table (cmd/koro
// wire_<module>.go, through the crypto wiring); the purpose of each value is
// rebuilt from a prefix and, optionally, a column of the same row (e.g.
// "identity.mfa_secret/" + user_id).
package rewrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"gorm.io/gorm"

	"koro/internal/platform/clock"
	"koro/internal/platform/crypto"
	"koro/internal/platform/db"
	"koro/internal/platform/queue"
)

// Job and schedule (01 §7 addition, ticket BE-055).
const (
	JobName = "crypto.rewrap"
	// Cron: hourly at :50 (Europe/Istanbul); a run stops after Budget and the
	// next one continues.
	Cron             = "50 * * * *"
	unique           = 55 * time.Minute
	DefaultBatchSize = 200
	DefaultBudget    = 8 * time.Minute
	jobTimeout       = 10 * time.Minute
)

var reIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

// Column is one envelope-encrypted column.
type Column struct {
	// Name identifies the column in logs ("identity.mfa_totp.secret_enc").
	Name string
	// Table, Key and Column are SQL identifiers: the table, its unique key
	// column (keyset order, normally the primary key) and the encrypted
	// bytea column.
	Table, Key, Column string
	// KeyType is the SQL type of Key ("uuid" when empty).
	KeyType string
	// PurposePrefix and PurposeColumn rebuild the envelope purpose of a row:
	// PurposePrefix + PurposeColumn::text (PurposeColumn may be empty).
	PurposePrefix string
	PurposeColumn string
}

// Validate checks the identifiers and the purpose rule.
func (c Column) Validate() error {
	for _, id := range []string{c.Table, c.Key, c.Column} {
		if !reIdent.MatchString(id) {
			return fmt.Errorf("rewrap: column %q: invalid SQL identifier %q", c.Name, id)
		}
	}
	if c.KeyType != "" && !reIdent.MatchString(c.KeyType) {
		return fmt.Errorf("rewrap: column %q: invalid key type %q", c.Name, c.KeyType)
	}
	if c.PurposeColumn != "" && !reIdent.MatchString(c.PurposeColumn) {
		return fmt.Errorf("rewrap: column %q: invalid SQL identifier %q", c.Name, c.PurposeColumn)
	}
	if c.Name == "" || c.PurposePrefix == "" && c.PurposeColumn == "" {
		return fmt.Errorf("rewrap: column %q needs a name and a purpose", c.Name)
	}
	return nil
}

// Envelope is what the job needs from crypto.Envelope.
type Envelope interface {
	ActiveKeyID(ctx context.Context) (string, error)
	Rewrap(ctx context.Context, purpose string, sealed []byte) ([]byte, bool, error)
}

// Deps are the job's dependencies.
type Deps struct {
	ORM      *gorm.DB
	Tx       db.TxManager
	Envelope Envelope
	Columns  []Column
	Clock    clock.Clock
	Log      *slog.Logger
	// BatchSize and Budget override the defaults (tests).
	BatchSize int
	Budget    time.Duration
}

// Job re-wraps the declared columns.
type Job struct{ d Deps }

// New validates d.
func New(d Deps) (*Job, error) {
	if d.ORM == nil || d.Tx == nil || d.Envelope == nil || d.Clock == nil || d.Log == nil {
		return nil, errors.New("rewrap: ORM, Tx, Envelope, Clock and Log are required")
	}
	seen := map[string]bool{}
	for _, c := range d.Columns {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if seen[c.Table+"."+c.Column] {
			return nil, fmt.Errorf("rewrap: column %s.%s declared twice", c.Table, c.Column)
		}
		seen[c.Table+"."+c.Column] = true
	}
	if d.BatchSize <= 0 || d.BatchSize > 1000 { // 01 §7: ≤ 1000 rows per transaction
		d.BatchSize = DefaultBatchSize
	}
	if d.Budget <= 0 {
		d.Budget = DefaultBudget
	}
	return &Job{d: d}, nil
}

// QueueJob is the scheduled job (low queue: maintenance).
func (j *Job) QueueJob() queue.Job {
	return queue.Job{
		Name: JobName, Cron: Cron, Queue: queue.Low, Unique: unique, Timeout: jobTimeout,
		Handle: func(ctx context.Context) error { _, err := j.Run(ctx); return err },
	}
}

// Result counts one run.
type Result struct {
	Rewrapped int  // values moved to the active key
	Conflicts int  // rows changed meanwhile (left for the next run)
	Failed    int  // values that could not be opened (left untouched, alarm)
	Complete  bool // every column was scanned to the end within the budget
}

// Run re-wraps until every column is done or the budget is spent.
func (j *Job) Run(ctx context.Context) (Result, error) {
	var res Result
	active, err := j.d.Envelope.ActiveKeyID(ctx)
	if err != nil {
		return res, fmt.Errorf("rewrap: %w", err)
	}
	deadline := j.d.Clock.Now().Add(j.d.Budget)
	res.Complete = true
	for _, c := range j.d.Columns {
		done, err := j.column(ctx, c, active, deadline, &res)
		if err != nil {
			j.summary(ctx, slog.LevelError, active, res, err)
			return res, err
		}
		if !done {
			res.Complete = false
			break
		}
	}
	level := slog.LevelInfo
	if res.Failed > 0 {
		level = slog.LevelError
	}
	j.summary(ctx, level, active, res, nil)
	return res, nil
}

func (j *Job) summary(ctx context.Context, level slog.Level, active string, res Result, err error) {
	attrs := []slog.Attr{
		slog.String("job", JobName), slog.String("active_key", active), slog.Int("rewrapped", res.Rewrapped),
		slog.Int("conflicts", res.Conflicts), slog.Int("failed", res.Failed), slog.Bool("complete", res.Complete),
	}
	if res.Failed > 0 {
		attrs = append(attrs, slog.Bool("alarm", true))
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	j.d.Log.LogAttrs(ctx, level, "crypto rewrap run", attrs...)
}

type row struct {
	ID      string `gorm:"column:id"`
	Sealed  []byte `gorm:"column:sealed"`
	Purpose string `gorm:"column:purpose"`
}

// column processes one column; done=false when the budget ran out.
func (j *Job) column(ctx context.Context, c Column, active string, deadline time.Time, res *Result) (bool, error) {
	after := ""
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !j.d.Clock.Now().Before(deadline) {
			return false, nil
		}
		rows, err := j.batch(ctx, c, active, after, first)
		if err != nil {
			return false, err
		}
		if len(rows) == 0 {
			return true, nil
		}
		first = false
		after = rows[len(rows)-1].ID

		type swap struct {
			id       string
			old, new []byte
		}
		var swaps []swap
		for _, r := range rows {
			purpose := c.PurposePrefix + r.Purpose
			out, changed, err := j.d.Envelope.Rewrap(ctx, purpose, r.Sealed)
			switch {
			case errors.Is(err, crypto.ErrUnavailable) || ctx.Err() != nil:
				return false, fmt.Errorf("rewrap %s: %w", c.Name, err) // fail closed: stop, retry later
			case err != nil:
				res.Failed++
				j.d.Log.LogAttrs(ctx, slog.LevelError, "crypto rewrap: value cannot be opened",
					slog.String("column", c.Name), slog.String("row_key", r.ID),
					slog.String("error", err.Error()), slog.Bool("alarm", true))
			case changed:
				swaps = append(swaps, swap{id: r.ID, old: r.Sealed, new: out})
			}
		}
		if len(swaps) == 0 {
			continue
		}
		keyType := c.KeyType
		if keyType == "" {
			keyType = "uuid"
		}
		q := fmt.Sprintf(`UPDATE %q SET %q = ? WHERE %q = CAST( ? AS %s ) AND %q = ?`, c.Table, c.Column, c.Key, keyType, c.Column)
		err = j.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			conn := db.Conn(ctx, j.d.ORM)
			for _, s := range swaps {
				r := conn.Exec(q, s.new, s.id, s.old)
				if r.Error != nil {
					return db.Translate(r.Error)
				}
				if r.RowsAffected == 1 {
					res.Rewrapped++
				} else {
					res.Conflicts++
				}
			}
			return nil
		})
		if err != nil {
			return false, fmt.Errorf("rewrap %s: update: %w", c.Name, err)
		}
	}
}

// batch selects the next values (keyset on Key) whose header names another
// key than active. The header is 0x01 ‖ len(kid) ‖ kid ‖ …; a value too
// short for a header is selected too (and reported as failed).
func (j *Job) batch(ctx context.Context, c Column, active, after string, first bool) ([]row, error) {
	purpose := "''"
	if c.PurposeColumn != "" {
		purpose = fmt.Sprintf("%q::text", c.PurposeColumn)
	}
	kid := fmt.Sprintf(`CASE WHEN octet_length(%[1]q) >= 2 THEN substring(%[1]q from 3 for get_byte(%[1]q, 1)) END`, c.Column)
	where := fmt.Sprintf(`%q IS NOT NULL AND %s IS DISTINCT FROM ?`, c.Column, kid)
	args := []any{[]byte(active)}
	keyType := c.KeyType
	if keyType == "" {
		keyType = "uuid"
	}
	if !first {
		where += fmt.Sprintf(` AND %q > CAST( ? AS %s )`, c.Key, keyType)
		args = append(args, after)
	}
	q := fmt.Sprintf(`SELECT %[1]q::text AS id, %[2]q AS sealed, %[3]s AS purpose FROM %[4]q WHERE %[5]s ORDER BY %[1]q LIMIT ?`,
		c.Key, c.Column, purpose, c.Table, where)
	args = append(args, j.d.BatchSize)
	var rows []row
	if err := db.Conn(ctx, j.d.ORM).Raw(q, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("rewrap %s: select: %w", c.Name, db.Translate(err))
	}
	return rows, nil
}
