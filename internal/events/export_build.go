package events

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/xuri/excelize/v2"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/clock"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
	"koro/internal/platform/textx"
	"koro/internal/students"
)

// ExportWarning is the first row of every export file (R-003 §2.6-5): the
// file carries personal data, may be used only for the stated reason and
// never handed on.
const ExportWarning = "KİŞİSEL VERİ: Bu dosya yalnız dışa aktarma gerekçesindeki amaç için kullanılabilir; " +
	"üçüncü kişilere aktarılamaz, işiniz bitince silin. Koro dışa aktarma no: "

// utf8BOM starts a CSV export (spreadsheets then read it as UTF-8).
const utf8BOM = "\uFEFF"

// exportSheet is the worksheet of an XLSX export.
const exportSheet = "Kayitlar"

// exportJob is the system actor of the build (the audit records of a
// ready or failed export).
const exportJob = BuildExportTaskName

// handleBuildExport is events.build_export: the queued export's file is
// written, stored outside any transaction (files.PutGenerated), then
// attached and the row made ready in one transaction. Idempotent: an
// export that is not queued any more is left alone, and a build that lost
// the race deletes its own file. A file over the purpose limit (50 MB)
// fails the export for good; any other error is retried.
func (s *service) handleBuildExport(ctx context.Context, payload []byte) error {
	var t buildTask
	if err := json.Unmarshal(payload, &t); err != nil {
		return queue.Permanent(fmt.Errorf("events: build export payload: %w", err))
	}
	if t.ExportID == uuid.Nil() {
		return queue.Permanent(errors.New("events: build export payload without an export id"))
	}
	x, err := s.d.Store.GetExport(ctx, t.OrgID, t.EventID, t.ExportID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // the row is gone (retention): nothing to build
	case err != nil:
		return err
	}
	if x.Status != ExportQueued {
		return nil
	}
	if !s.now().Before(x.ExpiresAt) {
		return s.failExport(ctx, x, "expired")
	}
	if s.d.Files == nil {
		return errors.New("events: files is not wired: export not built")
	}
	e, err := s.d.Store.GetEvent(ctx, x.EventID)
	if err != nil {
		return err
	}
	if e.OrgID != x.OrgID {
		return queue.Permanent(fmt.Errorf("events: export %s does not belong to its event's org", x.ID))
	}
	content, rows, err := s.renderExport(ctx, e, x)
	if err != nil {
		return err
	}
	ext, mime := "xlsx", files.MIMEXLSX
	if x.Format == FormatCSV {
		ext, mime = "csv", files.MIMECSV
	}
	org := x.OrgID
	fileID, err := s.d.Files.PutGenerated(ctx, files.GeneratedFile{
		Purpose: files.PurposeEventExport, OrgID: &org, ContentType: mime, Content: content,
		Name: "koro-katilimcilar-" + e.Slug + "-" + s.now().In(clock.Istanbul()).Format("20060102") + "." + ext,
	})
	switch {
	case errors.Is(err, files.ErrTooLarge):
		s.d.Log.LogAttrs(ctx, slog.LevelError, "event export too large for one file", slog.String("export_id", x.ID.String()),
			slog.Int("bytes", len(content)), slog.Int("rows", rows))
		return s.failExport(ctx, x, "too_large")
	case err != nil:
		return err
	}
	sum := sha256.Sum256(content)
	sys := authz.SystemActor(exportJob)
	stored := false
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockExport(ctx, x.OrgID, x.EventID, x.ID)
		if err != nil {
			return err
		}
		if cur.Status != ExportQueued { // a concurrent run won
			return s.d.Files.Delete(ctx, sys, fileID)
		}
		if err := s.d.Files.Attach(ctx, sys, fileID, files.Owner{Module: coverOwner, EntityID: x.ID, Purpose: files.PurposeEventExport}); err != nil {
			return err
		}
		n := rows
		cur.Status, cur.FileID, cur.FileSHA256, cur.RowCount = ExportReady, &fileID, sum[:], &n
		if _, err := s.d.Store.UpdateExport(ctx, cur); err != nil {
			return err
		}
		stored = true
		return s.d.Audit.Log(ctx, auditEvent(sys, authz.EventsExportCreate, exportAggType, x.ID, x.OrgID, "", map[string]any{
			"step": "ready", "event_id": x.EventID.String(), "file_id": fileID.String(), "row_count": rows, "bytes": len(content)}))
	})
	if err != nil {
		// Never leave the attendees' file to the 24-hour orphan cleanup.
		if derr := s.d.Files.Delete(context.WithoutCancel(ctx), sys, fileID); derr != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "unattached export file not removed", slog.String("export_id", x.ID.String()),
				slog.String("file_id", fileID.String()))
		}
		return err
	}
	if stored {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "event export ready", slog.String("export_id", x.ID.String()),
			slog.Int("rows", rows), slog.Int("bytes", len(content)))
	}
	return nil
}

// failExport turns a queued export into failed with its audit record
// (code: expired, too_large, stale).
func (s *service) failExport(ctx context.Context, x Export, code string) error {
	sys := authz.SystemActor(exportJob)
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockExport(ctx, x.OrgID, x.EventID, x.ID)
		if err != nil || cur.Status != ExportQueued {
			return err
		}
		cur.Status = ExportFailed
		if _, err := s.d.Store.UpdateExport(ctx, cur); err != nil {
			return err
		}
		return s.d.Audit.Log(ctx, audit.Event{
			Action: string(authz.EventsExportCreate), Outcome: audit.Failure, OrgID: &cur.OrgID, ResourceType: exportAggType,
			ResourceID: &cur.ID, Metadata: map[string]any{"step": "failed", "code": code, "event_id": cur.EventID.String()},
		}.By(sys))
	})
}

// exportCell renders one value of the export: text as is, times in
// Europe/Istanbul (RFC 3339), numbers and booleans as text, the values of
// a multi-select joined with "; ". Every cell goes through SafeCell (05
// §14.6: no formula injection).
func exportCell(v any) string {
	var s string
	switch x := v.(type) {
	case nil:
	case string:
		s = x
	case *string:
		if x != nil {
			s = *x
		}
	case time.Time:
		s = x.In(clock.Istanbul()).Format(time.RFC3339)
	case *time.Time:
		if x != nil {
			s = x.In(clock.Istanbul()).Format(time.RFC3339)
		}
	case bool:
		s = strconv.FormatBool(x)
	case float64:
		s = strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		s = x.String()
	case []any:
		parts := make([]string, 0, len(x))
		for _, p := range x {
			parts = append(parts, exportCell(p))
		}
		return textx.SafeCell(strings.Join(parts, "; "))
	default:
		s = fmt.Sprint(x)
	}
	return textx.SafeCell(s)
}

// exportCard is what an export needs of a registrant.
type exportCard struct {
	first, last, university, department, classYear string
}

// renderExport writes the file of x: the warning row, the header (the
// field names), then one row per registration of e (any status, oldest
// first). Names come from identity, university, department and class year
// from students and directory, only when a chosen field needs them; the
// answers are the stored, non-sensitive ones (02 §11.6: the
// special-category answers are never opened here).
func (s *service) renderExport(ctx context.Context, e Event, x Export) ([]byte, int, error) {
	fields := []string(x.Fields)
	need := func(names ...string) bool {
		return slices.ContainsFunc(fields, func(f string) bool { return slices.Contains(names, f) })
	}
	needNames, needStudent := need("first_name", "last_name"), need("university", "department", "class_year")
	unis, deps := map[uuid.UUID]string{}, map[uuid.UUID]string{}
	var data [][]string
	var after *After
	for {
		rows, err := s.d.Store.ListEventRegistrations(ctx, e.OrgID, e.ID, RegFilter{}, after, s.batch)
		if err != nil {
			return nil, 0, err
		}
		cards, err := s.exportCards(ctx, rows, needNames, needStudent, unis, deps)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			data = append(data, exportRow(fields, r, cards[r.UserID]))
		}
		if len(rows) < s.batch {
			break
		}
		last := rows[len(rows)-1]
		after = &After{At: last.CreatedAt, ID: last.ID}
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
	}
	header := make([]string, len(fields))
	for i, f := range fields {
		header[i] = textx.SafeCell(f)
	}
	warning := ExportWarning + x.ID.String()
	var (
		out []byte
		err error
	)
	if x.Format == FormatCSV {
		out, err = writeCSV(warning, header, data)
	} else {
		out, err = writeXLSX(warning, header, data)
	}
	if err != nil {
		return nil, 0, err
	}
	return out, len(data), nil
}

// exportRow is the row of r in the order of fields.
func exportRow(fields []string, r Registration, c exportCard) []string {
	var answers map[string]any
	row := make([]string, len(fields))
	for i, f := range fields {
		var v any
		switch f {
		case "registration_id":
			v = r.ID.String()
		case "status":
			v = string(r.Status)
		case "source":
			v = string(r.Source)
		case "created_at":
			v = r.CreatedAt
		case "confirmed_at":
			v = r.ConfirmedAt
		case "waitlisted_at":
			v = r.WaitlistedAt
		case "checked_in_at":
			v = r.CheckedInAt
		case "cancelled_at":
			v = r.CancelledAt
		case "first_name":
			v = c.first
		case "last_name":
			v = c.last
		case "university":
			v = c.university
		case "department":
			v = c.department
		case "class_year":
			v = c.classYear
		default:
			key, ok := strings.CutPrefix(f, AnswerFieldPrefix)
			if !ok {
				continue // not on the allow-list: never written (checked at create)
			}
			if answers == nil {
				answers = decodeAnswersNumber(r.Answers)
			}
			v = answers[key]
		}
		row[i] = exportCell(v)
	}
	return row
}

// decodeAnswersNumber decodes stored answers keeping numbers exact.
func decodeAnswersNumber(j JSON) map[string]any {
	out := map[string]any{}
	if len(j) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	_ = dec.Decode(&out)
	return out
}

// exportCards returns the cards of the registrants of rows (only the parts
// a chosen field needs; a missing piece stays empty). unis and deps cache
// the names across pages.
func (s *service) exportCards(ctx context.Context, rows []Registration, names, student bool, unis, deps map[uuid.UUID]string) (map[uuid.UUID]exportCard, error) {
	out := map[uuid.UUID]exportCard{}
	if len(rows) == 0 || (!names && !student) {
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		if !slices.Contains(ids, r.UserID) {
			ids = append(ids, r.UserID)
		}
	}
	if names && s.d.Users != nil {
		us, err := s.d.Users.GetMany(ctx, ids)
		if err != nil {
			return nil, err
		}
		for id, u := range us {
			out[id] = exportCard{first: u.FirstName, last: u.LastName}
		}
	}
	if !student || s.d.Students == nil {
		return out, nil
	}
	for _, id := range ids {
		p, err := s.d.Students.AudienceProfile(ctx, id)
		if err != nil {
			if errors.Is(err, students.ErrNotFound) {
				continue
			}
			return nil, err
		}
		c := out[id]
		if p.ClassYear != nil {
			c.classYear = strconv.Itoa(int(*p.ClassYear))
		}
		c.university = s.universityName(ctx, unis, p.UniversityID)
		if p.DepartmentID != nil {
			c.department = s.departmentName(ctx, deps, *p.DepartmentID)
		}
		out[id] = c
	}
	return out, nil
}

// writeCSV is the CSV file: UTF-8 with a byte order mark and CRLF line
// ends (spreadsheets open it with the Turkish letters intact).
func writeCSV(warning string, header []string, data [][]string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(utf8BOM)
	w := csv.NewWriter(&buf)
	w.UseCRLF = true
	if err := w.Write([]string{warning}); err != nil {
		return nil, err
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}
	if err := w.WriteAll(data); err != nil { // WriteAll flushes
		return nil, fmt.Errorf("events: export csv: %w", err)
	}
	return buf.Bytes(), nil
}

// writeXLSX is the XLSX file: one worksheet, every cell a string (no
// formula is ever written), streamed row by row.
func writeXLSX(warning string, header []string, data [][]string) (out []byte, err error) {
	f := excelize.NewFile()
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	if err := f.SetSheetName("Sheet1", exportSheet); err != nil {
		return nil, fmt.Errorf("events: export xlsx: %w", err)
	}
	sw, err := f.NewStreamWriter(exportSheet)
	if err != nil {
		return nil, fmt.Errorf("events: export xlsx: %w", err)
	}
	row := func(n int, cells []string) error {
		ref, err := excelize.CoordinatesToCellName(1, n)
		if err != nil {
			return err
		}
		vals := make([]any, len(cells))
		for i, c := range cells {
			vals[i] = excelize.Cell{Value: c}
		}
		return sw.SetRow(ref, vals)
	}
	if err := row(1, []string{warning}); err != nil {
		return nil, fmt.Errorf("events: export xlsx: %w", err)
	}
	if err := row(2, header); err != nil {
		return nil, fmt.Errorf("events: export xlsx: %w", err)
	}
	for i, d := range data {
		if err := row(i+3, d); err != nil {
			return nil, fmt.Errorf("events: export xlsx: %w", err)
		}
	}
	if err := sw.Flush(); err != nil {
		return nil, fmt.Errorf("events: export xlsx: %w", err)
	}
	buf, err := f.WriteToBuffer()
	if err != nil {
		return nil, fmt.Errorf("events: export xlsx: %w", err)
	}
	return buf.Bytes(), nil
}

// purgeExportsJob is events.purge_exports (01 §7).
func (s *service) purgeExportsJob() queue.Job {
	return queue.Job{Name: PurgeExportsJobName, Cron: purgeCron, Queue: queue.Low, Unique: purgeUnique,
		Handle: func(ctx context.Context) error { _, err := s.purgeExports(ctx); return err }}
}

// purgeExports deletes the file of every ready export past expires_at
// (one transaction per export: status purged, file_id cleared, the file
// deleted, audited as events.export.purge and recorded in destruction_logs
// as event_export_file, one row per file; T-318), queues the build of exports
// whose task was lost and fails the ones queued for a day. It never waits
// for the legal approval of event_export_file: the file lives 7 days (06
// H-48 pattern). It returns how many files it deleted.
func (s *service) purgeExports(ctx context.Context) (int, error) {
	sys := authz.SystemActor(PurgeExportsJobName)
	if err := authz.Can(ctx, sys, authz.EventsExportPurge, authz.Resource{}); err != nil {
		return 0, queue.Permanent(err)
	}
	now := s.now()
	runID := uuid.NewV7()
	purged, after := 0, uuid.Nil()
	for {
		rows, err := s.d.Store.ExportsDue(ctx, now, after, s.batch)
		if err != nil {
			return purged, err
		}
		for _, x := range rows {
			after = x.ID
			ok, err := s.purgeExport(ctx, sys, x, now, runID)
			if err != nil {
				return purged, err
			}
			if ok {
				purged++
			}
		}
		if len(rows) < s.batch {
			break
		}
	}
	if err := s.repairQueued(ctx, now); err != nil {
		return purged, err
	}
	if purged > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "expired event exports deleted", slog.Int("count", purged))
	}
	return purged, nil
}

// purgeExport deletes the file of x under its row lock; false when another
// run got there first. The destruction_logs row (T-318) is written in the
// same transaction: the status change makes a repeated run a no-op, so a
// file is recorded once, and a failed record rolls the deletion back.
func (s *service) purgeExport(ctx context.Context, sys authz.Actor, x Export, now time.Time, runID uuid.UUID) (bool, error) {
	if s.d.Files == nil {
		return false, errors.New("events: files is not wired: export files not purged")
	}
	if s.d.Destructions == nil {
		return false, errors.New("events: destruction record is not wired: export files not purged")
	}
	started := s.now()
	done := false
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockExport(ctx, x.OrgID, x.EventID, x.ID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		if cur.Status != ExportReady || cur.ExpiresAt.After(now) || cur.FileID == nil {
			return nil
		}
		fileID := *cur.FileID
		cur.Status, cur.FileID, cur.PurgedAt = ExportPurged, nil, &now
		if _, err := s.d.Store.UpdateExport(ctx, cur); err != nil {
			return err
		}
		if err := s.d.Files.Delete(ctx, sys, fileID); err != nil {
			return err
		}
		done = true
		if err := s.d.Audit.Log(ctx, auditEvent(sys, authz.EventsExportPurge, exportAggType, cur.ID, cur.OrgID, "", map[string]any{
			"policy_code": PolicyExportFile, "event_id": cur.EventID.String(), "file_id": fileID.String()})); err != nil {
			return err
		}
		return s.d.Destructions.Record(ctx, privacy.Destruction{PolicyCode: PolicyExportFile, RunID: runID, Module: "events",
			Target: "files:" + string(files.PurposeEventExport), Method: privacy.MethodFileDelete, Count: 1, PeriodCutoff: now, StartedAt: started,
			FinishedAt: s.now(), Details: map[string]any{"job": PurgeExportsJobName, "file_id": fileID.String()}})
	})
	return done, err
}

// repairQueued queues the build of exports queued for more than
// ExportRequeueAfter (mechanism A is best effort, 01 §6.1: the task ID
// deduplicates a build that is merely waiting) and fails the ones queued
// for more than ExportGiveUpAfter.
func (s *service) repairQueued(ctx context.Context, now time.Time) error {
	after := uuid.Nil()
	for {
		rows, err := s.d.Store.QueuedExports(ctx, now.Add(-ExportRequeueAfter), after, s.batch)
		if err != nil {
			return err
		}
		for _, x := range rows {
			after = x.ID
			if !x.CreatedAt.After(now.Add(-ExportGiveUpAfter)) {
				if err := s.failExport(ctx, x, "stale"); err != nil {
					return err
				}
				continue
			}
			if s.d.Queue == nil {
				continue
			}
			t := buildTaskOf(x)
			if err := s.d.Queue.Enqueue(ctx, BuildExportTaskName, t, t.dedupID(), queue.Queue(queue.Default),
				queue.Timeout(buildTimeout)); err != nil {
				return err
			}
		}
		if len(rows) < s.batch {
			return nil
		}
	}
}
