package ingest

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/orgs"
	"koro/internal/platform/problem"
	"koro/internal/platform/ratelimit"
	"koro/internal/platform/textx"
)

// The quick import (BE-211; research kulup-etkinlik-toplama-2026-11-11,
// Aşama 2). Club events live mostly on Instagram, which Koro never
// fetches; instead the club's admin pastes the text of the club's own post
// and/or gives its poster, the extractor reads the fields of a draft event
// and the admin corrects them and creates the event with the ordinary
// createEvent. Nothing is stored here: no imported_events row, no events
// draft, no image (a poster is an ordinary event_cover upload of the club:
// it stays only if the club makes it its event's cover, files' orphan
// cleanup removes it otherwise).
//
// Order of the checks: the club (404), authz (ingest.quick_import.run:
// the club's club_admin, as events.event.create), the club's status, the
// input and the posters' metadata (422/409, nothing spent), then the club's
// hourly and daily limits and the platform's daily budget (the language
// model's cost; 429 with Retry-After, fail closed), then the posters' bytes
// and the extractor.
//
// Personal data: the text is masked before it leaves (mask.go), as in the
// pipeline. A poster cannot be masked in code (no local OCR or face
// detection), so a poster with faces or names would be a transfer abroad
// (KVKK md. 9): reading posters is a switch (INGEST_QUICK_IMPORT_IMAGES:
// auto = dev/staging on, prod off until the lawyer's review); files has
// already removed every metadata (EXIF, GPS) by re-encoding the image; the
// model is told never to put a person in a field, and the answer is masked
// again in code (validateDraft). Every call is audited without content
// (ingest.quick_import.run: the file IDs, the number of drafts).

// QuickImportInput is a quick import: the post text, the posters, or both.
type QuickImportInput struct {
	Text    string
	FileIDs []uuid.UUID
}

// QuickImportResult is the preview: the drafts in the extractor's order.
type QuickImportResult struct {
	Drafts []QuickDraft
	// ImagesRead is false when posters were given but the extractor could
	// not read them (the development extractor).
	ImagesRead bool
}

// Rate limit rules of the quick import (the limiter's keys:
// rl:<name>:<key>).
const (
	ruleQuickImportOrgHour = "ingest_quick_import_org_hour"
	ruleQuickImportOrgDay  = "ingest_quick_import_org_day"
	ruleQuickImportDay     = "ingest_quick_import_day"
)

// resourceOrg is the audit resource type of a quick import (the club).
const resourceOrg = "org"

// QuickImport implements Service.
func (s *service) QuickImport(ctx context.Context, a authz.Actor, orgID uuid.UUID, in QuickImportInput) (QuickImportResult, error) {
	o, res, err := s.clubOrg(ctx, orgID)
	if err != nil {
		return QuickImportResult{}, err
	}
	if err := authz.Can(ctx, a, authz.IngestQuickImportRun, res); err != nil {
		return QuickImportResult{}, err
	}
	if err := clubWritable(o); err != nil {
		return QuickImportResult{}, err
	}
	text, err := quickText(in.Text)
	if err != nil {
		return QuickImportResult{}, err
	}
	if text == "" && len(in.FileIDs) == 0 {
		return QuickImportResult{}, problem.Field("body", "give the post text, the poster (file_ids) or both")
	}
	if err := s.checkPosters(ctx, orgID, in.FileIDs); err != nil {
		return QuickImportResult{}, err
	}
	if err := s.spendQuickImport(ctx, orgID); err != nil {
		s.m.quick(quickRateLimited)
		return QuickImportResult{}, err
	}
	images, err := s.readPosters(ctx, in.FileIDs)
	if err != nil {
		return QuickImportResult{}, err
	}
	masked := maskPersonalData(text)
	today := s.now().In(s.loc)
	ex, err := s.d.Extractor.Draft(ctx, DraftRequest{Text: masked, Images: images, Today: today, Club: o.Name})
	s.m.quickInputTokens.Add(int64(ex.InputTokens))
	s.m.quickOutputTokens.Add(int64(ex.OutputTokens))
	out := QuickImportResult{ImagesRead: len(images) == 0 || ex.ImagesRead}
	outcome, failed := audit.Success, error(nil)
	switch {
	case errors.Is(err, ErrExtractRefused):
		// The model declined or gave no usable answer: an empty preview
		// (the club types the event itself), not an outage.
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "quick import unreadable", slog.String("org_id", orgID.String()),
			slog.String("err", err.Error()))
		s.m.quick(quickUnreadable)
	case err != nil:
		s.d.Log.LogAttrs(ctx, slog.LevelError, "quick import extractor failed", slog.String("org_id", orgID.String()),
			slog.String("extractor", s.d.Extractor.Name()), slog.String("err", err.Error()))
		s.m.quick(quickFailed)
		// Still audited: the posters may have reached the provider.
		outcome, failed = audit.Failure, problem.ErrUnavailable.Wrap(err)
	default:
		for _, c := range ex.Drafts {
			if len(out.Drafts) == maxDrafts {
				break
			}
			if d, ok := validateDraft(c, masked, len(images) > 0, today, s.loc); ok {
				out.Drafts = append(out.Drafts, d)
			}
		}
		if len(out.Drafts) == 0 {
			s.m.quick(quickEmpty)
		} else {
			s.m.quick(quickOK)
		}
	}
	ids := make([]string, len(in.FileIDs))
	for i, id := range in.FileIDs {
		ids[i] = id.String()
	}
	if err := s.d.Audit.Log(ctx, audit.Event{
		Action: string(authz.IngestQuickImportRun), Outcome: outcome, ResourceType: resourceOrg, ResourceID: &orgID,
		OrgID: &orgID, Metadata: map[string]any{"text_chars": utf8.RuneCountInString(text), "file_ids": ids,
			"drafts": len(out.Drafts), "images_read": out.ImagesRead, "extractor": s.d.Extractor.Name()},
	}.By(a)); err != nil {
		return QuickImportResult{}, errors.Join(failed, err)
	}
	if failed != nil {
		return QuickImportResult{}, failed
	}
	return out, nil
}

// clubWritable is the org status rule of writes on behalf of the club (03
// §2.3): only an active club prepares events (createEvent would refuse a
// suspended or closed one), so its quick import spends nothing otherwise.
// The codes are orgs' (06 H-52).
func clubWritable(o orgs.OrgSummary) error {
	switch o.Status {
	case orgs.StatusActive:
		return nil
	case orgs.StatusSuspended:
		return orgs.ErrOrgSuspended
	}
	return orgs.ErrInvalidState
}

// quickText normalises the pasted text: valid UTF-8 without control or
// bidi characters (newlines and tabs kept as line breaks and spaces), at
// most maxDraftTextRunes.
func quickText(raw string) (string, error) {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	var kept []string
	for _, l := range lines {
		n, err := textx.NormalizeText(l)
		if err != nil {
			return "", problem.Field("body.text", "contains control or bidi characters")
		}
		kept = append(kept, n)
	}
	text := strings.TrimSpace(strings.Join(kept, "\n"))
	if utf8.RuneCountInString(text) > maxDraftTextRunes {
		return "", problem.Field("body.text", "must be at most "+strconv.Itoa(maxDraftTextRunes)+" characters")
	}
	return text, nil
}

// checkPosters checks, before anything is spent, that every poster is a
// completed event_cover upload of the club (06 H-38: the club comes from
// the file row), not rejected, retired or infected, small enough, and that
// reading posters is on. A poster still waiting for its scan is files'
// 409 not_clean (retry).
func (s *service) checkPosters(ctx context.Context, orgID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > maxDraftImages {
		return problem.Field("body.file_ids", "must hold at most "+strconv.Itoa(maxDraftImages)+" posters")
	}
	if !s.d.Config.QuickImportImages {
		return ErrQuickImportImagesOff
	}
	if s.d.Files == nil {
		return errUnavailable
	}
	for i, id := range ids {
		loc := "body.file_ids[" + strconv.Itoa(i) + "]"
		if slices.Index(ids, id) != i {
			return problem.Field(loc, "is given twice")
		}
		f, err := s.d.Files.Get(ctx, id)
		switch {
		case errors.Is(err, files.ErrNotFound):
			return problem.Field(loc, "must be an event_cover upload of this club")
		case err != nil:
			return err
		}
		switch {
		case f.Purpose != files.PurposeEventCover || f.OrgID == nil || *f.OrgID != orgID:
			return problem.Field(loc, "must be an event_cover upload of this club")
		case f.Status == files.StatusRejected || f.Retired || f.ScanStatus == files.ScanInfected:
			return files.ErrRejected
		case f.Status != files.StatusActive:
			return problem.Field(loc, "the upload has not completed")
		case f.ScanStatus == files.ScanPending:
			return files.ErrNotClean
		case f.SizeBytes > maxDraftImageBytes:
			return problem.Field(loc, "must be at most "+strconv.Itoa(maxDraftImageBytes>>20)+" MB")
		}
	}
	return nil
}

// readPosters reads the posters' bytes (files serves only clean objects;
// the main bucket holds the re-encoded image without metadata).
func (s *service) readPosters(ctx context.Context, ids []uuid.UUID) ([]DraftImage, error) {
	out := make([]DraftImage, 0, len(ids))
	for _, id := range ids {
		b, f, err := s.d.Files.Read(ctx, id, maxDraftImageBytes)
		if err != nil {
			return nil, err
		}
		out = append(out, DraftImage{MediaType: f.MIME, Data: b})
	}
	return out, nil
}

// spendQuickImport takes one unit of the club's hourly and daily limits
// and of the platform's daily budget. An unavailable limiter fails closed
// (503): the budget bounds a cost. A spent one is 429 with the wait.
func (s *service) spendQuickImport(ctx context.Context, orgID uuid.UUID) error {
	if s.d.Limiter == nil {
		return errUnavailable
	}
	c := s.d.Config
	org := "org:" + orgID.String()
	for _, r := range []struct {
		rule ratelimit.Rule
		key  string
	}{
		{ratelimit.Rule{Name: ruleQuickImportOrgHour, Rate: c.QuickImportOrgHourly, Per: time.Hour, Key: ratelimit.KeyOrg, Strict: true}, org},
		{ratelimit.Rule{Name: ruleQuickImportOrgDay, Rate: c.QuickImportOrgDaily, Per: 24 * time.Hour, Key: ratelimit.KeyOrg, Strict: true}, org},
		{ratelimit.Rule{Name: ruleQuickImportDay, Rate: c.QuickImportDailyBudget, Per: 24 * time.Hour, Strict: true}, "platform"},
	} {
		res, err := s.d.Limiter.Allow(ctx, r.rule, r.key)
		switch {
		case err != nil:
			s.d.Log.LogAttrs(ctx, slog.LevelError, "quick import limiter unavailable (fail closed)",
				slog.String("rule", r.rule.Name), slog.String("err", err.Error()))
			return problem.ErrUnavailable.Wrap(err)
		case !res.Allowed:
			level := slog.LevelInfo
			if r.rule.Name == ruleQuickImportDay {
				level = slog.LevelWarn // the platform's budget is spent: every club waits
			}
			s.d.Log.LogAttrs(ctx, level, "quick import rate limited", slog.String("rule", r.rule.Name),
				slog.String("org_id", orgID.String()))
			return problem.ErrRateLimited.WithRetryAfter(res.RetryAfter)
		}
	}
	return nil
}
