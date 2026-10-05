package seed

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// batchSize is the number of statements sent in one round trip.
const batchSize = 500

// Stmt is a statement the plan runs after its rows (a back reference that
// needs both rows, e.g. event_forms.current_version_id). It must be
// idempotent itself.
type Stmt struct {
	SQL  string
	Args []any
}

// Result is what Apply did: the inserted rows per table (a row that was
// there already counts as existing).
type Result struct {
	Inserted map[string]int
	Existing map[string]int
}

// Total returns the inserted and existing row counts.
func (r Result) Total() (inserted, existing int) {
	for _, n := range r.Inserted {
		inserted += n
	}
	for _, n := range r.Existing {
		existing += n
	}
	return inserted, existing
}

// Apply writes the plan in one transaction (an advisory lock keeps two
// runs from interleaving). Every row is `INSERT … ON CONFLICT DO NOTHING`
// with its deterministic ID, so a second run inserts nothing (idempotent)
// and never touches what the demo users changed since.
func Apply(ctx context.Context, pool *pgxpool.Pool, p *Plan) (Result, error) {
	res := Result{Inserted: map[string]int{}, Existing: map[string]int{}}
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('koro.seed.demo'))`); err != nil {
			return fmt.Errorf("seed: lock: %w", err)
		}
		for start := 0; start < len(p.Rows); start += batchSize {
			rows := p.Rows[start:min(start+batchSize, len(p.Rows))]
			b := &pgx.Batch{}
			for _, r := range rows {
				sql, args := insertSQL(r)
				b.Queue(sql, args...)
			}
			br := tx.SendBatch(ctx, b)
			for _, r := range rows {
				tag, err := br.Exec()
				if err != nil {
					_ = br.Close()
					return fmt.Errorf("seed: insert into %s: %w", r.Table, err)
				}
				if tag.RowsAffected() == 1 {
					res.Inserted[r.Table]++
				} else {
					res.Existing[r.Table]++
				}
			}
			if err := br.Close(); err != nil {
				return fmt.Errorf("seed: batch: %w", err)
			}
		}
		for _, s := range p.After {
			if _, err := tx.Exec(ctx, s.SQL, s.Args...); err != nil {
				return fmt.Errorf("seed: %w", err)
			}
		}
		return nil
	})
	return res, err
}

// insertSQL is the statement of one row.
func insertSQL(r Row) (string, []any) {
	ph := make([]string, len(r.Vals))
	args := make([]any, len(r.Vals))
	for i, v := range r.Vals {
		ph[i] = "$" + strconv.Itoa(i+1)
		if t, ok := v.(typed); ok {
			ph[i] += "::" + t.typ
			v = t.v
		}
		args[i] = v
	}
	return "INSERT INTO " + r.Table + " (" + strings.Join(r.Cols, ", ") + ") VALUES (" + strings.Join(ph, ", ") +
		") ON CONFLICT DO NOTHING", args
}
