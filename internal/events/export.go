package events

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

// Registration exports (BE-050; 01 §4.10, §7, 02 §11.9, 03 §5.6, §6.2, 04
// §2.6, R-003 §2.6-5, T-37, T-69):
//
//   - createEventExport (events.export.create, S·R): the organiser's
//     managers ask for a file of the event's registrations with a reason
//     (10–500 characters) and the fields they need from an allow-list
//     (ExportBaseFields and answers.<key> of the event's form; never an
//     e-mail, T-69). include_sensitive is refused (422; T-37: the
//     special-category answers never leave). Order: the event (404) →
//     authz (404/403) → the body (422) → the org status (03 §2.3) → the
//     re-authentication proof (403; a refused proof is one denied record,
//     an overload none: audit.ReauthFailed, BE-074) → the queued row, its
//     audit record and the build task after the commit (202). Rate class
//     export (5/hour/actor, strict).
//   - events.build_export (worker): the file (CSV or XLSX; every cell
//     through textx.SafeCell, the first row a personal-data warning) is
//     stored with files.PutGenerated, attached to the export and the row
//     becomes ready with its SHA-256 and row count.
//   - listEventExports (events.export.list): the event's exports, newest
//     first; the organiser's managers and the DPO.
//   - downloadEventExport (events.export.download, S·O): only the
//     requester (still a manager of the organiser) downloads, only a ready
//     export before expires_at (409 events.export_not_ready); every
//     download counts (download_count, first_downloaded_at) and is audited
//     before the presigned URL (≤ 5 minutes) is returned (03 §6.5).
//   - events.purge_exports (hourly :25): a ready export's file is deleted
//     at expires_at (created_at + event_export_file, 7 days) in the
//     transaction that clears file_id (status purged, purged_at) with its
//     destruction_logs row (kvkk through hooks.destruction; T-318); lost
//     build tasks are queued again, builds that never ran give up after a
//     day (failed). The row is the 2-year accountability record
//     (event_export_log, the events sweeper).
const (
	// BuildExportTaskName builds the file of a queued export (mechanism A).
	BuildExportTaskName = "events.build_export"
	// PurgeExportsJobName deletes expired export files (01 §7: hourly :25,
	// low; never deferred to the legal approval: the file lives 7 days).
	PurgeExportsJobName = "events.purge_exports"

	// PolicyExportFile is the lifetime of an export file (02 §24.2;
	// expires_at = created_at + this period, 7 days by default).
	PolicyExportFile = "event_export_file"
	// PolicyExportLog is the retention of the export rows (2 years).
	PolicyExportLog = "event_export_log"

	// ExportLifetime is the default of PolicyExportFile.
	ExportLifetime = 7 * 24 * time.Hour
	// ExportDownloadTTL is the life of a download URL (03 §5.6: ≤ 5 min).
	ExportDownloadTTL = 5 * time.Minute
	// ExportRequeueAfter: a queued export older than this gets its build
	// task again (a lost after-commit enqueue; the task ID deduplicates).
	ExportRequeueAfter = 15 * time.Minute
	// ExportGiveUpAfter: a queued export older than this is failed.
	ExportGiveUpAfter = 24 * time.Hour

	// AnswerFieldPrefix names an answer column: answers.<field key>.
	AnswerFieldPrefix = "answers."

	minExportReason = 10
	maxExportReason = 500
	maxExportFields = 40
	buildTimeout    = 10 * time.Minute
	purgeCron       = "25 * * * *"
	purgeUnique     = 55 * time.Minute
)

// ExportBaseFields is the allow-list of the registration columns an export
// may carry (04 §V.6 RegistrationOrgDTO without the avatar, the decision
// note and the consent snapshot). There is no e-mail, phone or birth date
// column (T-69; the registrant's contact stays with Koro).
func ExportBaseFields() []string {
	return []string{"registration_id", "status", "source", "created_at", "confirmed_at", "waitlisted_at", "checked_in_at",
		"cancelled_at", "first_name", "last_name", "university", "department", "class_year"}
}

// exportResource is the authz view of an export: the event's resource
// (its visibility and the caller's staff role) as an event_export owned by
// the requester (03 §5.6 download: S·O).
func exportResource(ev authz.Resource, x Export) authz.Resource {
	by := x.RequestedBy
	ev.Type, ev.ID, ev.OwnerUserID, ev.Attrs = authz.TypeEventExport, x.ID, &by, maps.Clone(ev.Attrs)
	return ev
}

// exportAggType is the audit resource type of exports.
const exportAggType = authz.TypeEventExport

// buildTask is the payload of events.build_export (IDs only, 05 §8.4).
type buildTask struct {
	ExportID uuid.UUID `json:"export_id"`
	EventID  uuid.UUID `json:"event_id"`
	OrgID    uuid.UUID `json:"org_id"`
}

func buildTaskOf(x Export) buildTask {
	return buildTask{ExportID: x.ID, EventID: x.EventID, OrgID: x.OrgID}
}

// dedupID is the task ID of an export's build: a repeated enqueue (the
// purge job's repair) never builds twice.
func (t buildTask) dedupID() string { return BuildExportTaskName + ":" + t.ExportID.String() }

// CreateExport implements Service.
func (s *service) CreateExport(ctx context.Context, a authz.Actor, eventID uuid.UUID, in ExportInput, proof identity.ReauthProof) (Export, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return Export{}, err
	}
	if err := s.authorize(ctx, a, authz.EventsExportCreate, e); err != nil {
		return Export{}, err
	}
	in, err = s.cleanExport(ctx, e, in)
	if err != nil {
		return Export{}, err
	}
	o, err := s.org(ctx, e.OrgID)
	if err != nil {
		return Export{}, err
	}
	// A durable copy of the attendees' data leaves the platform: the org
	// status rule of writes applies (03 §2.3).
	if err := writable(a, o); err != nil {
		return Export{}, err
	}
	if s.d.Reauth == nil || s.d.Files == nil || s.d.Queue == nil {
		return Export{}, problem.ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		// A refused proof is one denied record of the export to be (the
		// org from the event row read before the proof, 06 H-38); an
		// overload writes nothing (03 §6.2, BE-074).
		org := e.OrgID
		denied := audit.Event{Action: string(authz.EventsExportCreate), ResourceType: exportAggType, OrgID: &org}.By(a)
		return Export{}, audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	hash, err := idempotency.Hash(struct {
		Op    string
		Event uuid.UUID
		In    ExportInput
	}{"create_export", e.ID, in})
	if err != nil {
		return Export{}, err
	}
	now := s.now()
	life, err := s.retention(ctx, PolicyExportFile, ExportLifetime)
	if err != nil {
		return Export{}, err
	}
	id, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (uuid.UUID, error) {
		x, err := s.d.Store.InsertExport(ctx, Export{
			ID: uuid.NewV7(), EventID: e.ID, OrgID: e.OrgID, RequestedBy: a.UserID, Reason: in.Reason, Fields: in.Fields,
			Format: in.Format, Status: ExportQueued, IP: clientIP(ctx), ExpiresAt: now.Add(life),
		})
		if err != nil {
			return uuid.Nil(), err
		}
		meta := map[string]any{"event_id": e.ID.String(), "fields": slices.Clone(in.Fields), "format": string(in.Format),
			audit.MetaReauth: kind}
		if err := s.d.Audit.Log(ctx, auditEvent(a, authz.EventsExportCreate, exportAggType, x.ID, x.OrgID, in.Reason, meta)); err != nil {
			return uuid.Nil(), err
		}
		t := buildTaskOf(x)
		s.d.Queue.AfterCommit(ctx, BuildExportTaskName, t, t.dedupID(), queue.Queue(queue.Default), queue.Timeout(buildTimeout))
		return x.ID, nil
	})
	if err != nil {
		return Export{}, err
	}
	return s.d.Store.GetExport(ctx, e.OrgID, e.ID, id)
}

// cleanExport checks the body of createEventExport: include_sensitive is
// refused (T-37), the reason is normalised text of 10–500 characters, the
// fields are distinct names of the allow-list (base fields, answers.<key>
// of a non-sensitive, non-consent field of a published version), the
// format csv or xlsx (default xlsx).
func (s *service) cleanExport(ctx context.Context, e Event, in ExportInput) (ExportInput, error) {
	var errs []problem.FieldError
	add := func(loc, msg string) { errs = append(errs, problem.FieldError{Location: loc, Message: msg}) }
	if in.IncludeSensitive {
		add("body.include_sensitive", "special-category answers are never exported (T-37); leave it false")
	}
	reason, err := cleanText("body.reason", in.Reason, minExportReason, maxExportReason)
	if err != nil {
		add("body.reason", "must be "+strconv.Itoa(minExportReason)+"-"+strconv.Itoa(maxExportReason)+
			" characters without control characters")
	}
	switch in.Format {
	case "":
		in.Format = FormatXLSX
	case FormatCSV, FormatXLSX:
	default:
		add("body.format", "must be csv or xlsx")
	}
	answers, err := s.exportableAnswers(ctx, e)
	if err != nil {
		return ExportInput{}, err
	}
	switch n := len(in.Fields); {
	case n == 0:
		add("body.fields", "choose at least one field")
	case n > maxExportFields:
		add("body.fields", "at most "+strconv.Itoa(maxExportFields)+" fields")
	}
	base := ExportBaseFields()
	seen := map[string]bool{}
	for i, f := range in.Fields {
		loc := "body.fields[" + strconv.Itoa(i) + "]"
		key, isAnswer := strings.CutPrefix(f, AnswerFieldPrefix)
		switch {
		case seen[f]:
			add(loc, "is listed twice")
		case strings.Contains(strings.ToLower(f), "mail"):
			add(loc, "e-mail addresses are never exported (T-69)")
		case isAnswer && !slices.Contains(answers, key):
			add(loc, "is not an exportable answer of this event's form (special-category and consent fields are never exported)")
		case !isAnswer && !slices.Contains(base, f):
			add(loc, "is not an exportable field")
		}
		seen[f] = true
	}
	if len(errs) > 0 {
		return ExportInput{}, problem.ErrValidation.WithFields(errs...)
	}
	in.Reason, in.Fields = reason, slices.Clone(in.Fields)
	return in, nil
}

// exportableAnswers are the answer keys an export may name: the fields of
// every published version of e's form that are neither consent fields nor
// warn_special in any version (their answers live encrypted and never
// leave; T-37), sorted.
func (s *service) exportableAnswers(ctx context.Context, e Event) ([]string, error) {
	f, err := s.d.Store.GetForm(ctx, e.OrgID, e.ID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, err
	}
	vs, err := s.d.Store.ListFormVersions(ctx, e.OrgID, f.ID)
	if err != nil {
		return nil, err
	}
	keys, banned := map[string]bool{}, map[string]bool{}
	for _, v := range vs {
		schema, err := DecodeFormSchema(v.Schema)
		if err != nil {
			return nil, err
		}
		rep, err := DecodeLintReport(v.LintReport)
		if err != nil {
			return nil, err
		}
		for _, k := range rep.SensitiveKeys() {
			banned[k] = true
		}
		for _, fl := range schema.Fields {
			if fl.Type == FieldConsent {
				banned[fl.Key] = true
				continue
			}
			keys[fl.Key] = true
		}
	}
	var out []string
	for k := range keys {
		if !banned[k] {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, nil
}

// clientIP is the canonical client address of the request (nil when
// unknown: a worker, a test).
func clientIP(ctx context.Context) *string {
	a, err := netip.ParseAddr(strings.TrimSpace(httpx.ClientIPFrom(ctx)))
	if err != nil {
		return nil
	}
	ip := a.Unmap().WithZone("").String()
	return &ip
}

// ListExports implements Service.
func (s *service) ListExports(ctx context.Context, a authz.Actor, eventID uuid.UUID, limit int, after *After) (ExportPage, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return ExportPage{}, err
	}
	if err := s.authorize(ctx, a, authz.EventsExportList, e); err != nil {
		return ExportPage{}, err
	}
	n := page.ClampLimit(limit)
	rows, err := s.d.Store.ListExports(ctx, e.OrgID, e.ID, after, n+1)
	if err != nil {
		return ExportPage{}, err
	}
	rows, more := page.Cut(rows, n)
	out := ExportPage{Items: rows}
	if more && len(rows) > 0 {
		last := rows[len(rows)-1]
		out.Next = &After{At: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// DownloadExport implements Service. Order: the event (404) → authz on
// the event (the exports are its sub-surface: 404 invisible, 403 without
// the role) → the export of this event (404, 06 H-38) → authz on the
// export (403 for a manager who did not request it, S·O) → ready and not
// expired (409) → the URL → the counted download and its audit record in
// one transaction; the URL leaves only after both (03 §6.5).
func (s *service) DownloadExport(ctx context.Context, a authz.Actor, eventID, exportID uuid.UUID) (ExportDownload, error) {
	e, err := s.d.Store.GetEvent(ctx, eventID)
	if err != nil {
		return ExportDownload{}, err
	}
	res, err := s.resource(ctx, a, e)
	if err != nil {
		return ExportDownload{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsExportDownload, res); err != nil {
		return ExportDownload{}, err
	}
	x, err := s.d.Store.GetExport(ctx, e.OrgID, e.ID, exportID)
	if err != nil {
		return ExportDownload{}, err
	}
	if err := authz.Can(ctx, a, authz.EventsExportDownload, exportResource(res, x)); err != nil {
		return ExportDownload{}, err
	}
	now := s.now()
	if x.Status != ExportReady || x.FileID == nil || !now.Before(x.ExpiresAt) {
		return ExportDownload{}, ErrExportNotReady
	}
	if s.d.Files == nil {
		return ExportDownload{}, problem.ErrUnavailable
	}
	dl, err := s.d.Files.PresignGet(ctx, *x.FileID, ExportDownloadTTL)
	switch {
	case errors.Is(err, files.ErrNotFound):
		return ExportDownload{}, ErrExportNotReady // purged meanwhile
	case err != nil:
		return ExportDownload{}, err
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.RecordDownload(ctx, x.OrgID, x.EventID, x.ID, now)
		switch {
		case errors.Is(err, ErrNotFound):
			return ErrExportNotReady // purged meanwhile
		case err != nil:
			return err
		}
		return s.d.Audit.Log(ctx, auditEvent(a, authz.EventsExportDownload, exportAggType, x.ID, x.OrgID, "", map[string]any{
			"event_id": x.EventID.String(), "file_id": x.FileID.String(), "download_count": cur.DownloadCount,
			"ttl_seconds": int(ExportDownloadTTL / time.Second)}))
	})
	if err != nil {
		return ExportDownload{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "event export download url issued", slog.String("export_id", x.ID.String()),
		slog.String("event_id", x.EventID.String()))
	return ExportDownload{URL: dl.URL, ExpiresAt: dl.ExpiresAt}, nil
}
