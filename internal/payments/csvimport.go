package payments

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/money"
	"koro/internal/platform/problem"
)

// The bank statement import (adminImportIncomingTransfers; 04 §2.16 asks
// for a file_id, but files has no purpose that accepts a CSV upload — T-294
// — so the statement travels as CSV text in the JSON body).
//
// Format: UTF-8 (a BOM is ignored), the first line is the header, the
// delimiter is ";" or "," (whichever the header uses more). Columns, by
// header name in any order (case-insensitive): value_date (YYYY-MM-DD or
// DD.MM.YYYY / DD/MM/YYYY), amount (TL: "1234,56", "1.234,56", "1234.56";
// a debit — negative — is rejected), description, bank_reference and the
// optional sender_name and sender_iban. Every data row is imported,
// skipped as a duplicate (the account already has its bank_reference,
// another row of the file included) or rejected with a fixed reason; a
// file that is not valid CSV or misses a column is 422 as a whole.

// Import limits.
const (
	MaxImportBytes = 1 << 20 // 1 MiB of CSV text
	MaxImportRows  = 2000
)

// Import columns.
const (
	colValueDate     = "value_date"
	colAmount        = "amount"
	colDescription   = "description"
	colBankReference = "bank_reference"
	colSenderName    = "sender_name"
	colSenderIBAN    = "sender_iban"
)

var (
	requiredColumns = []string{colValueDate, colAmount, colDescription, colBankReference}
	knownColumns    = []string{colValueDate, colAmount, colDescription, colBankReference, colSenderName, colSenderIBAN}
)

// statementRow is one parsed data row; err is its rejection reason.
type statementRow struct {
	line  int
	draft transferDraft
	err   string
}

// parseStatement reads the CSV text into rows (422 body.csv for a file that
// cannot be read as a statement).
func parseStatement(text string) ([]statementRow, error) {
	bad := func(msg string) error { return problem.Field("body.csv", msg) }
	if len(text) > MaxImportBytes {
		return nil, bad(fmt.Sprintf("must be at most %d bytes", MaxImportBytes))
	}
	text = strings.TrimPrefix(text, "\ufeff")
	head, _, _ := strings.Cut(text, "\n")
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = ','
	if strings.Count(head, ";") > strings.Count(head, ",") {
		r.Comma = ';'
	}
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, bad("must start with a header line")
	}
	cols := map[string]int{}
	for i, h := range header {
		name := strings.ToLower(strings.TrimSpace(h))
		if _, dup := cols[name]; dup && name != "" {
			return nil, bad("names a column twice: " + name)
		}
		cols[name] = i
	}
	for _, c := range requiredColumns {
		if _, ok := cols[c]; !ok {
			return nil, bad("needs the columns " + strings.Join(requiredColumns, ", ") + " (sender_name and sender_iban are optional)")
		}
	}
	var rows []statementRow
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		var pe *csv.ParseError
		switch {
		case errors.As(err, &pe):
			return nil, bad(fmt.Sprintf("line %d is not valid CSV", pe.Line))
		case err != nil:
			return nil, bad("is not valid CSV")
		}
		line, _ := r.FieldPos(0)
		if len(rows) == MaxImportRows {
			return nil, bad(fmt.Sprintf("must have at most %d rows", MaxImportRows))
		}
		rows = append(rows, rowOf(line, rec, cols))
	}
	if len(rows) == 0 {
		return nil, bad("has no rows")
	}
	return rows, nil
}

// rowOf reads one data record.
func rowOf(line int, rec []string, cols map[string]int) statementRow {
	row := statementRow{line: line}
	field := func(name string) string {
		i, ok := cols[name]
		if !ok || i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}
	maxCol := 0
	for _, c := range knownColumns {
		if i, ok := cols[c]; ok && i > maxCol {
			maxCol = i
		}
	}
	if len(rec) <= maxCol {
		row.err = "the row has fewer columns than the header"
		return row
	}
	d, err := parseDate(field(colValueDate))
	if err != nil {
		row.err = "value_date: must be a date (YYYY-MM-DD or DD.MM.YYYY)"
		return row
	}
	amount, err := parseAmount(field(colAmount))
	if err != nil {
		row.err = "amount: must be a positive TL amount (e.g. 1.234,56)"
		return row
	}
	row.draft = transferDraft{ValueDate: d, Amount: amount, Description: field(colDescription),
		BankReference: field(colBankReference), SenderName: field(colSenderName), SenderIBAN: field(colSenderIBAN)}
	return row
}

// parseDate reads YYYY-MM-DD, DD.MM.YYYY or DD/MM/YYYY.
func parseDate(s string) (civil.Date, error) {
	if len(s) == len("02.01.2006") && (s[2] == '.' && s[5] == '.' || s[2] == '/' && s[5] == '/') {
		s = s[6:] + "-" + s[3:5] + "-" + s[:2]
	}
	return civil.Parse(s)
}

// parseAmount reads a positive TL amount into kuruş. The last "." or ","
// followed by one or two digits is the decimal separator; every other one
// groups thousands ("1.234.567,89", "1,234.50", "1234"). Signs, spaces
// inside the number and three-decimal fractions are rejected.
func parseAmount(s string) (int64, error) {
	bad := errors.New("not an amount")
	if s == "" || strings.ContainsAny(s, "-+ ") {
		return 0, bad
	}
	whole, frac := s, ""
	if i := strings.LastIndexAny(s, ".,"); i >= 0 && (len(s)-i-1 == 1 || len(s)-i-1 == 2) {
		whole, frac = s[:i], s[i+1:]
	}
	whole = strings.NewReplacer(".", "", ",", "").Replace(whole)
	if whole == "" {
		whole = "0"
	}
	num := whole
	if frac != "" {
		num += "." + frac
	}
	v, err := money.ParseDecimal(num)
	if err != nil || v <= 0 {
		return 0, bad
	}
	return v, nil
}

// rowError is the fixed reason of a draft the validation refused (its first
// field error, never a value).
func rowError(err error) string {
	var pe *problem.Error
	if errors.As(err, &pe) {
		if fs := pe.Fields(); len(fs) > 0 {
			return fs[0].Location + ": " + fs[0].Message
		}
		return pe.Title()
	}
	return "the row is not valid"
}

// ImportTransfers implements Service (fin; Idempotency-Key): authz (403) →
// the statement (422 for a file that is not one) → the account (approved;
// 422) → every row validated (a bad row is rejected, not fatal) → in one
// transaction: each valid row recorded unmatched (csv_import), a row whose
// bank reference the account already has skipped as a duplicate → one
// audit record with the counts. A retry with the same key and statement
// replays the first answer; a new key re-imports nothing (row-level
// uniqueness).
func (s *service) ImportTransfers(ctx context.Context, a authz.Actor, in ImportInput) (ImportResult, error) {
	if err := authz.Can(ctx, a, authz.PaymentsTransferCreate, adminSurface); err != nil {
		return ImportResult{}, err
	}
	rows, err := parseStatement(in.CSV)
	if err != nil {
		return ImportResult{}, err
	}
	if _, err := s.recordingAccount(ctx, "body.collection_account_id", in.CollectionAccountID); err != nil {
		return ImportResult{}, err
	}
	for i, r := range rows {
		if r.err != "" {
			continue
		}
		d, err := s.cleanDraft("", r.draft)
		if err != nil {
			rows[i].err = rowError(err)
			continue
		}
		rows[i].draft = d
	}
	sum := sha256.Sum256([]byte(in.CSV))
	hash, err := idempotency.Hash(struct {
		Op      string
		Account uuid.UUID
		CSV     []byte
	}{"import_transfers", in.CollectionAccountID, sum[:]})
	if err != nil {
		return ImportResult{}, err
	}
	res, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (ImportResult, error) {
		res := ImportResult{Rows: make([]ImportRow, 0, len(rows))}
		for _, r := range rows {
			if r.err != "" {
				res.Rejected++
				res.Rows = append(res.Rows, ImportRow{Line: r.line, Outcome: RowRejected, Error: r.err})
				continue
			}
			stored, inserted, err := s.d.Store.InsertTransfer(ctx, s.newTransfer(a, in.CollectionAccountID, r.draft, EntryCSVImport))
			switch {
			case err != nil:
				return ImportResult{}, err
			case !inserted:
				res.Duplicates++
				res.Rows = append(res.Rows, ImportRow{Line: r.line, Outcome: RowDuplicate})
			default:
				res.Imported++
				id := stored.ID
				res.Rows = append(res.Rows, ImportRow{Line: r.line, Outcome: RowImported, TransferID: &id})
			}
		}
		err := s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.PaymentsTransferCreate), Outcome: audit.Success, ResourceType: authz.TypeIncomingTransfer,
			Metadata: map[string]any{"entry_method": string(EntryCSVImport), "collection_account_id": in.CollectionAccountID.String(),
				"imported": res.Imported, "duplicates": res.Duplicates, "rejected": res.Rejected},
		}.By(a))
		return res, err
	})
	if err != nil {
		return ImportResult{}, err
	}
	return res, nil
}
