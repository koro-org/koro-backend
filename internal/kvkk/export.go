package kvkk

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/clock"
	"koro/internal/platform/privacy"
	"koro/internal/platform/problem"
)

// ExportFormat names the layout of the export zip (manifest.json "format").
const ExportFormat = "koro-dsr-export/1"

// Export zip layout:
//
//	manifest.json        format, request, generation time, one entry per section
//	<module>.json        {"module": ..., "items": [...]} of every registered provider
//	talent_report.json   profiling_access only: the auditor report, or
//	                     {"available": false} while talent is not wired
const (
	manifestName     = "manifest.json"
	talentReportName = "talent_report.json"
)

// manifest is manifest.json.
type manifest struct {
	Format      string            `json:"format"`
	DSRID       string            `json:"dsr_id"`
	Type        string            `json:"type"`
	GeneratedAt time.Time         `json:"generated_at"`
	Sections    []manifestSection `json:"sections"`
}

type manifestSection struct {
	File   string `json:"file"`
	Module string `json:"module"`
	Items  int    `json:"items"`
}

// providers returns the registered providers sorted by name; a name
// registered twice is a wiring error.
func (s *service) providers() ([]privacy.Provider, error) {
	ps := slices.Clone(s.d.Providers())
	slices.SortFunc(ps, func(a, b privacy.Provider) int { return strings.Compare(a.Name(), b.Name()) })
	for i := 1; i < len(ps); i++ {
		if ps[i].Name() == ps[i-1].Name() {
			return nil, fmt.Errorf("kvkk: privacy provider %q registered twice", ps[i].Name())
		}
	}
	return ps, nil
}

// exportZip builds the export of d: every registered provider's section
// (access, export) or the talent auditor report (profiling_access).
func (s *service) exportZip(ctx context.Context, d DSR) ([]byte, error) {
	uid := *d.UserID
	ctx = privacy.WithSubject(ctx, privacy.Subject{UserID: uid, DSRID: d.ID})
	at := s.now()
	type part struct {
		name, module string
		items        int
		body         any
	}
	var parts []part
	if d.Type == TypeProfilingAccess {
		body, err := s.talentReport(ctx, d)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part{name: talentReportName, module: "talent", items: 1, body: body})
	} else {
		ps, err := s.providers()
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			sec, err := p.Export(ctx, uid)
			if err != nil {
				return nil, fmt.Errorf("kvkk: export %s: %w", p.Name(), err)
			}
			items := sec.Items
			if items == nil {
				items = []map[string]any{}
			}
			parts = append(parts, part{name: p.Name() + ".json", module: p.Name(), items: len(items),
				body: map[string]any{"module": p.Name(), "items": items}})
		}
	}
	m := manifest{Format: ExportFormat, DSRID: d.ID.String(), Type: string(d.Type), GeneratedAt: at, Sections: []manifestSection{}}
	for _, p := range parts {
		m.Sections = append(m.Sections, manifestSection{File: p.name, Module: p.module, Items: p.items})
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	add := func(name string, v any) error {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return fmt.Errorf("kvkk: export %s: %w", name, err)
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: at.In(clock.Istanbul())})
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	}
	if err := add(manifestName, m); err != nil {
		return nil, err
	}
	for _, p := range parts {
		if err := add(p.name, p.body); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("kvkk: export zip: %w", err)
	}
	return buf.Bytes(), nil
}

// talentReport is the auditor report of a profiling_access request, asked
// for on behalf of its first approver (talent audits it as talent.read).
func (s *service) talentReport(ctx context.Context, d DSR) (any, error) {
	if s.d.Talent == nil || !s.d.Talent.Available() {
		return map[string]any{"available": false,
			"note": "Koro has no profiling (talent) data processing in operation for this account."}, nil
	}
	by := uuid.Nil()
	if d.FirstApprover != nil {
		by = *d.FirstApprover
	}
	raw, err := s.d.Talent.Report(ctx, by, *d.UserID, "dsr:"+d.ID.String())
	if err != nil {
		return nil, fmt.Errorf("kvkk: talent report: %w", err)
	}
	return map[string]any{"available": true, "report": raw}, nil
}

// exportJob is the system actor of the export and purge steps.
const (
	exportJob = "kvkk.finish_request"
	purgeJob  = "kvkk.purge_exports"
)

// buildExport stores the export of a completed request and attaches it.
// It is idempotent: a request with a file, without an account or past its
// retention is left alone; a concurrent build that lost the race deletes
// its own file.
func (s *service) buildExport(ctx context.Context, d DSR) error {
	switch {
	case d.UserID == nil, d.ExportFileID != nil, d.Status != StatusCompleted, !d.Type.exports():
		return nil
	case d.CompletedAt != nil && s.now().Sub(*d.CompletedAt) >= s.d.ExportRetention:
		return nil // a redelivered event must not rebuild a purged export
	}
	content, err := s.exportZip(ctx, d)
	if err != nil {
		return err
	}
	fileID, err := s.d.Files.PutGenerated(ctx, files.GeneratedFile{
		Purpose: files.PurposeDSRExport, OwnerUserID: d.UserID,
		Name:        "koro-kisisel-veriler-" + s.now().In(clock.Istanbul()).Format("20060102") + ".zip",
		ContentType: files.MIMEZIP, Content: content,
	})
	if err != nil {
		if errors.Is(err, files.ErrTooLarge) {
			s.d.Log.LogAttrs(ctx, slog.LevelError, "data export too large for one file", slog.String("dsr_id", d.ID.String()),
				slog.Int("bytes", len(content)), slog.Bool("alarm", true))
		}
		return err
	}
	sys := authz.SystemActor(exportJob)
	stored := false
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.Lock(ctx, d.ID)
		if err != nil {
			return err
		}
		if cur.ExportFileID != nil { // a concurrent run won
			return s.d.RemoveFile(ctx, sys, fileID)
		}
		if err := s.d.Files.Attach(ctx, sys, fileID, files.Owner{Module: "kvkk", EntityID: d.ID, Purpose: files.PurposeDSRExport}); err != nil {
			return err
		}
		cur.ExportFileID = &fileID
		if err := s.d.Store.Update(ctx, cur); err != nil {
			return err
		}
		stored = true
		return s.d.Audit.Log(ctx, event(sys, authz.KvkkDsrComplete, cur, map[string]any{
			"step": "export_ready", "file_id": fileID.String(), "bytes": len(content)}))
	})
	if err != nil {
		// Do not leave the personal file to the 24-hour orphan cleanup.
		if rerr := s.d.RemoveFile(context.WithoutCancel(ctx), sys, fileID); rerr != nil {
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "unattached export file not removed", slog.String("dsr_id", d.ID.String()),
				slog.String("file_id", fileID.String()))
		}
		return err
	}
	if stored {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "data export ready", slog.String("dsr_id", d.ID.String()),
			slog.String("file_id", fileID.String()), slog.Int("bytes", len(content)))
	}
	return nil
}

// download returns a presigned URL of d's export (≤ 5 minutes) and audits
// the download in its own transaction (03 §5.22: every download is
// audited; the URL is returned only once the audit row is written).
func (s *service) download(ctx context.Context, a authz.Actor, d DSR) (Download, error) {
	now := s.now()
	if d.ExportFileID == nil {
		if d.Status == StatusCompleted && d.Type.exports() && d.CompletedAt != nil && now.Sub(*d.CompletedAt) >= s.d.ExportRetention {
			return Download{}, ErrExportExpired
		}
		return Download{}, ErrExportNotReady
	}
	meta, err := s.d.Files.Get(ctx, *d.ExportFileID)
	switch {
	case errors.Is(err, problem.ErrNotFound):
		return Download{}, ErrExportExpired
	case err != nil:
		return Download{}, err
	case now.Sub(meta.CreatedAt) >= s.d.ExportRetention:
		return Download{}, ErrExportExpired // the hourly purge has not run yet
	}
	dl, err := s.d.Files.PresignGet(ctx, *d.ExportFileID, DownloadTTL)
	if err != nil {
		return Download{}, err
	}
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return s.d.Audit.Log(ctx, event(a, authz.KvkkDsrExportDownload, d, map[string]any{
			"file_id": d.ExportFileID.String(), "ttl_seconds": int(DownloadTTL / time.Second)}))
	})
	if err != nil {
		return Download{}, err
	}
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, "data export download url issued", slog.String("dsr_id", d.ID.String()),
		slog.String("file_id", d.ExportFileID.String()))
	return Download{URL: dl.URL, ExpiresAt: dl.ExpiresAt}, nil
}
