package ingest

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
	"uuid"

	"koro/internal/directory"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// fetchJob is the system actor of the runs (audit of the imported clubs).
const fetchJob = "ingest.fetch_due"

// oneOffLease is how long a one-off source (a sitemap's page) waits after
// a failed run before it is tried again.
const oneOffLease = time.Hour

// TickResult summarises one fetch_due run.
type TickResult struct {
	Runs    int // sources fetched (or refused by robots.txt)
	Skipped int // due sources left for a later tick (host gap, budget)
}

// RunDue implements Service: the due sources, earliest first, at most one
// per host per tick and none of a host fetched less than the gap (the
// host's Crawl-delay when longer) ago, until the tick budget is spent.
func (s *service) RunDue(ctx context.Context) (TickResult, error) {
	var res TickResult
	cfg := s.d.Config
	if !cfg.FetchEnabled {
		return res, nil
	}
	start := s.d.Clock.Now()
	due, err := s.d.Store.DueSources(ctx, s.now(), cfg.TickBatch)
	if err != nil {
		return res, err
	}
	hosts := make([]string, 0, len(due))
	for _, src := range due {
		hosts = append(hosts, src.Host)
	}
	last, err := s.d.Store.HostLastFetched(ctx, hosts)
	if err != nil {
		return res, err
	}
	used := map[string]bool{}
	for _, src := range due {
		if ctx.Err() != nil || s.d.Clock.Now().Sub(start) > cfg.TickBudget {
			res.Skipped += len(due) - res.Runs - res.Skipped
			break
		}
		if used[src.Host] || s.tooSoon(last, src.Host, cfg.HostGap) {
			res.Skipped++
			continue
		}
		used[src.Host] = true
		adapter := s.d.Adapters[src.Adapter]
		if adapter == nil || adapter.Accept() == nil {
			if err := s.finishUnfetched(ctx, src, OutcomeUnsupported, "", nil); err != nil {
				return res, err
			}
			res.Runs++
			continue
		}
		verdict, err := s.d.Fetcher.Robots(ctx, src.URL)
		var outcome Outcome
		var code string
		var pause *PausedReason
		switch {
		case errors.Is(err, ErrBlocked):
			outcome = OutcomeBlocked
		case err != nil, verdict.Unreachable:
			outcome, code = OutcomeFetchError, "robots_unreachable"
		case !verdict.Allowed:
			outcome, pause = OutcomeRobotsDenied, ptr(PausedRobots)
		}
		if outcome != "" {
			if err := s.finishUnfetched(ctx, src, outcome, code, pause); err != nil {
				return res, err
			}
			res.Runs++
			continue
		}
		if verdict.CrawlDelay > cfg.HostGap && s.tooSoon(last, src.Host, verdict.CrawlDelay) {
			res.Skipped++
			continue
		}
		claimed, ok, err := s.claim(ctx, src.ID)
		if err != nil {
			return res, err
		}
		if !ok {
			continue // another tick took it, or an admin changed it
		}
		if err := s.runSource(ctx, claimed, adapter); err != nil {
			return res, err
		}
		res.Runs++
	}
	return res, nil
}

// tooSoon reports whether host was fetched less than gap ago.
func (s *service) tooSoon(last map[string]time.Time, host string, gap time.Duration) bool {
	t, ok := last[host]
	return ok && s.d.Clock.Now().Sub(t) < gap
}

// claim takes a due source for this run: still enabled and due, its next
// run moved on (a concurrent tick skips it) and last_fetched_at set now
// (the host gap of every other tick).
func (s *service) claim(ctx context.Context, id uuid.UUID) (Source, bool, error) {
	var out Source
	ok := false
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		src, err := s.d.Store.LockSource(ctx, id)
		if errors.Is(err, errNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		now := s.now()
		if !src.Enabled || src.NextRunAt == nil || src.NextRunAt.After(now) {
			return nil
		}
		next := now.Add(max(src.Interval(), oneOffLease))
		src.NextRunAt, src.LastFetchedAt = &next, &now
		if err := s.d.Store.UpdateSource(ctx, src); err != nil {
			return err
		}
		out, ok = src, true
		return nil
	})
	return out, ok, err
}

// runState is what one run learned, written by finish.
type runState struct {
	run          Run
	hash         []byte
	etag, lastMd string
	yield        int
	pause        *PausedReason
}

// runSource fetches, parses and stores one claimed source.
func (s *service) runSource(ctx context.Context, src Source, adapter SourceAdapter) error {
	st := runState{run: Run{ID: uuid.NewV7(), SourceID: src.ID, StartedAt: s.now()}}
	page, err := s.d.Fetcher.Fetch(ctx, FetchRequest{
		URL: src.URL, ETag: deref(src.ETag), LastModified: deref(src.LastModified), Accept: adapter.Accept(),
	})
	if page.Status != 0 {
		status := page.Status
		st.run.HTTPStatus = &status
	}
	st.run.Bytes = len(page.Body)
	switch {
	case errors.Is(err, ErrBlocked):
		st.run.Outcome = OutcomeBlocked
	case err != nil:
		st.run.Outcome, st.run.ErrorCode = OutcomeFetchError, ptr(fetchErrorCode(err))
	case page.NotModified:
		st.run.Outcome = OutcomeNotModified
	case page.Status < 200 || page.Status > 299:
		st.run.Outcome = OutcomeHTTPError
	}
	if st.run.Outcome != "" {
		return s.finish(ctx, src, st)
	}
	st.etag, st.lastMd = page.ETag, page.LastModified
	hash := bodyHash(page.Body)
	if bytes.Equal(hash, src.ContentHash) {
		st.run.Outcome, st.hash = OutcomeUnchanged, hash
		return s.finish(ctx, src, st)
	}
	parsed, err := adapter.Parse(ctx, src, page)
	if err != nil {
		st.run.Outcome = OutcomeParseError
		return s.finish(ctx, src, st)
	}
	switch adapter.Kind() {
	case KindClubList:
		s.sinkClubs(ctx, src, parsed.Clubs, &st)
	default:
		if err := s.sinkChildren(ctx, src, parsed.Links, &st); err != nil {
			return err
		}
		if len(parsed.Docs) > 0 {
			if err := s.extractEvents(ctx, src, parsed.Docs, &st); err != nil {
				st.run.Outcome, st.run.ErrorCode = OutcomeExtractError, ptr("extractor")
				s.d.Log.LogAttrs(ctx, slog.LevelWarn, "ingest extraction failed", slog.String("source_id", src.ID.String()),
					slog.String("extractor", s.d.Extractor.Name()), slog.String("err", err.Error()))
				return s.finish(ctx, src, st)
			}
		}
	}
	st.run.Outcome, st.hash = OutcomeOK, hash
	return s.finish(ctx, src, st)
}

// fetchErrorCode names a fetch failure for the run log (no URL, no body).
func fetchErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrTooLarge):
		return "too_large"
	case errors.Is(err, ErrContentType):
		return "content_type"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "network"
}

// finishUnfetched records a run that fetched nothing (unsupported,
// blocked, robots), pausing the source when pause is set.
func (s *service) finishUnfetched(ctx context.Context, src Source, o Outcome, code string, pause *PausedReason) error {
	now := s.now()
	st := runState{run: Run{ID: uuid.NewV7(), SourceID: src.ID, StartedAt: now, Outcome: o}, pause: pause}
	if code != "" {
		st.run.ErrorCode = &code
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockSource(ctx, src.ID)
		if errors.Is(err, errNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		next := now.Add(max(cur.Interval(), oneOffLease))
		cur.NextRunAt = &next
		return s.finishTx(ctx, cur, st)
	})
}

// finish writes the run and the source's state in one transaction.
func (s *service) finish(ctx context.Context, src Source, st runState) error {
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		cur, err := s.d.Store.LockSource(ctx, src.ID)
		if errors.Is(err, errNotFound) {
			return nil // deleted while it ran
		}
		if err != nil {
			return err
		}
		return s.finishTx(ctx, cur, st)
	})
}

// finishTx applies one run to a locked source.
func (s *service) finishTx(ctx context.Context, cur Source, st runState) error {
	now := s.now()
	st.run.FinishedAt = now
	if st.run.FinishedAt.Before(st.run.StartedAt) {
		st.run.FinishedAt = st.run.StartedAt
	}
	o := st.run.Outcome
	s.m.run(o)
	code := string(o)
	cur.LastStatus = st.run.HTTPStatus
	switch {
	case !o.Failed():
		cur.FailCount, cur.LastOKAt, cur.LastError = 0, &now, nil
		if st.etag != "" {
			cur.ETag = ptr(clip(st.etag, 200))
		}
		if st.lastMd != "" {
			cur.LastModified = ptr(clip(st.lastMd, 100))
		}
		if st.hash != nil {
			cur.ContentHash = st.hash
		}
		if o == OutcomeOK {
			s.yieldWatch(ctx, &cur, st.yield)
		}
		if cur.IntervalSeconds == 0 && o != OutcomeUnsupported {
			cur.NextRunAt = nil // a one-off page is done
		}
	default:
		cur.FailCount++
		cur.LastError = &code
		if st.pause == nil && cur.FailCount >= s.d.Config.FailLimit {
			reason := PausedFailures
			st.pause = &reason
			s.m.pausedByFailures.Add(1)
			s.d.Log.LogAttrs(ctx, slog.LevelError, "ingest source paused after repeated failures", slog.Bool("alarm", true),
				slog.String("source_id", cur.ID.String()), slog.String("host", cur.Host), slog.String("outcome", code),
				slog.Int("fail_count", cur.FailCount))
		}
	}
	if st.pause != nil {
		cur.Enabled, cur.PausedReason = false, st.pause
	}
	if err := s.d.Store.UpdateSource(ctx, cur); err != nil {
		return err
	}
	return s.d.Store.InsertRun(ctx, st.run)
}

// yieldWatch keeps the yield of a successful run and raises the "getiri
// sıfırlandı" alarm when a source that yielded comes back empty
// ZeroYieldAlarm times in a row (the site's structure changed).
func (s *service) yieldWatch(ctx context.Context, cur *Source, yield int) {
	prev := cur.LastYield
	cur.LastYield = &yield
	if yield > 0 {
		cur.ZeroYieldRuns = 0
		return
	}
	cur.ZeroYieldRuns++
	if cur.ZeroYieldRuns == s.d.Config.ZeroYieldAlarm && (prev == nil || *prev == 0) {
		s.m.zeroYieldAlarms.Add(1)
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "ingest source yields nothing anymore", slog.Bool("alarm", true),
			slog.String("source_id", cur.ID.String()), slog.String("host", cur.Host),
			slog.String("adapter", string(cur.Adapter)), slog.Int("empty_runs", cur.ZeroYieldRuns))
	}
}

// sinkChildren adds the pages a sitemap named as one-off sources.
func (s *service) sinkChildren(ctx context.Context, src Source, links []Link, st *runState) error {
	for _, l := range links {
		u, host, err := ParseSourceURL(l.URL)
		if err != nil || host != src.Host {
			continue
		}
		now := s.now()
		parent := src.ID
		_, created, err := s.d.Store.InsertSource(ctx, Source{
			ID: uuid.NewV7(), Kind: KindEvents, Adapter: AdapterHTMLPage, URL: u, Host: host, Label: src.Label,
			UniversityID: src.UniversityID, ClubOrgID: src.ClubOrgID, ParentID: &parent, Enabled: true, NextRunAt: &now,
		})
		if err != nil {
			return err
		}
		st.run.Found++
		if created {
			st.run.Created++
		}
	}
	st.yield += len(links)
	return nil
}

// extractEvents masks the documents, asks the extractor, validates every
// candidate and stores the valid ones.
func (s *service) extractEvents(ctx context.Context, src Source, docs []Document, st *runState) error {
	uni, err := s.d.Directory.GetUniversity(ctx, src.UniversityID)
	if err != nil {
		return err
	}
	docs = s.prepareDocs(src, docs)
	today := s.d.Clock.Now().In(s.loc)
	x, err := s.d.Extractor.Extract(ctx, ExtractRequest{Docs: docs, Today: today, University: uni.Name})
	s.m.inputTokens.Add(int64(x.InputTokens))
	s.m.outputTokens.Add(int64(x.OutputTokens))
	st.run.InputTokens += x.InputTokens
	st.run.OutputTokens += x.OutputTokens
	if err != nil {
		return err
	}
	retention, err := s.eventRetention(ctx)
	if err != nil {
		return err
	}
	for _, c := range x.Candidates {
		st.run.Found++
		ev, reason := validateCandidate(c, docs, today, s.loc, s.d.Config.MinConfidence)
		if reason != "" {
			st.run.Rejected++
			s.m.rejected.Add(1)
			continue
		}
		created, err := s.sinkEvent(ctx, src, docs[ev.Doc].URL, ev, retention)
		switch {
		case err != nil && problem.IsClientError(err):
			st.run.Rejected++
			s.m.rejected.Add(1)
		case err != nil:
			return err
		case created:
			st.run.Created++
			s.m.created.Add(1)
		default:
			st.run.Updated++
		}
	}
	st.yield += st.run.Created + st.run.Updated
	return nil
}

// prepareDocs masks every document and keeps them within MaxTextChars (a
// document cut at the limit, the rest dropped).
func (s *service) prepareDocs(src Source, docs []Document) []Document {
	budget := s.d.Config.MaxTextChars
	out := make([]Document, 0, len(docs))
	for _, d := range docs {
		if budget <= 0 {
			break
		}
		d.Text = maskPersonalData(d.Text)
		if r := []rune(d.Text); len(r) > budget {
			d.Text = string(r[:budget])
		}
		budget -= len([]rune(d.Text))
		if src.ClubOrgID != nil && d.Organizer == "" {
			d.Organizer = src.Label
		}
		out = append(out, d)
	}
	return out
}

// eventRetention is the imported_event period (policy, else config).
func (s *service) eventRetention(ctx context.Context) (time.Duration, error) {
	if s.d.EventRetention != nil {
		d, err := s.d.EventRetention(ctx)
		if err != nil {
			return 0, err
		}
		if d > 0 {
			return d, nil
		}
	}
	return s.d.Config.EventRetention, nil
}

// sinkEvent stores one validated event: the club of the source, else the
// club the page names (its imported_clubs key), else the university; one
// row per fingerprint (a second sighting completes it).
func (s *service) sinkEvent(ctx context.Context, src Source, sourceURL string, ev ValidEvent, retention time.Duration) (bool, error) {
	club := src.ClubOrgID
	if club == nil && ev.Organizer != "" {
		ic, err := s.d.Store.FindImportedClub(ctx, src.UniversityID, directory.ClubNameKey(ev.Organizer))
		switch {
		case err == nil:
			club = &ic.ClubOrgID
		case !errors.Is(err, errNotFound):
			return false, err
		}
	}
	fp := fingerprint(src.UniversityID, club, ev.StartsAt, ev.Title)
	e := ImportedEvent{
		ID: uuid.NewV7(), SourceID: &src.ID, UniversityID: src.UniversityID, ClubOrgID: club, Status: StatusImported,
		Title: ev.Title, StartsAt: ev.StartsAt.UTC(), EndsAt: db.UTCPtr(ev.EndsAt), AllDay: ev.AllDay, Category: ev.Category,
		SourceURL: clip(sourceURL, 2048), Evidence: ev.Evidence, Confidence: round2(ev.Confidence), Fingerprint: fp,
		LastSeenAt: s.now(),
	}
	if ev.Location != "" {
		e.LocationText = &ev.Location
	}
	e.ExpiresAt = e.End().Add(retention)
	created := false
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		old, err := s.d.Store.EventByFingerprint(ctx, fp)
		switch {
		case errors.Is(err, errNotFound):
			created = true
			return s.d.Store.InsertEvent(ctx, e)
		case err != nil:
			return err
		}
		return s.d.Store.RefreshEvent(ctx, refreshed(old, e))
	})
	if err != nil && db.IsUnique(err) {
		return false, nil // a concurrent run stored it first
	}
	return created, err
}

// refreshed completes an event with a later sighting: an end, a place or
// a category it lacked, the later expiry and last_seen_at.
func refreshed(old, seen ImportedEvent) ImportedEvent {
	if old.EndsAt == nil && seen.EndsAt != nil {
		old.EndsAt = seen.EndsAt
	}
	if old.LocationText == nil {
		old.LocationText = seen.LocationText
	}
	if old.Category == CategoryOther && seen.Category != CategoryOther {
		old.Category = seen.Category
	}
	if seen.ExpiresAt.After(old.ExpiresAt) {
		old.ExpiresAt = seen.ExpiresAt
	}
	old.LastSeenAt = seen.LastSeenAt
	return old
}

// sinkClubs imports the entries of an official list, one transaction per
// club (a refused entry never stops the list), then marks the clubs the
// list no longer names.
func (s *service) sinkClubs(ctx context.Context, src Source, clubs []ClubCandidate, st *runState) {
	seenSince := st.run.StartedAt
	for _, c := range clubs {
		st.run.Found++
		created, err := s.importClub(ctx, src, c)
		switch {
		case err != nil:
			st.run.Rejected++
			s.d.Log.LogAttrs(ctx, slog.LevelWarn, "ingest club entry refused", slog.String("source_id", src.ID.String()),
				slog.String("err", err.Error()))
		case created:
			st.run.Created++
			s.m.clubsCreated.Add(1)
		default:
			st.run.Updated++
			s.m.clubsMatched.Add(1)
		}
	}
	st.yield += st.run.Created + st.run.Updated
	if n, err := s.d.Store.MarkMissingClubs(ctx, src.ID, seenSince, s.now()); err != nil {
		s.d.Log.LogAttrs(ctx, slog.LevelWarn, "ingest missing clubs not marked", slog.String("source_id", src.ID.String()),
			slog.String("err", err.Error()))
	} else if n > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "ingest clubs left the official list", slog.String("source_id", src.ID.String()),
			slog.Int("clubs", n))
	}
}

// importClub opens or matches one club and keeps its imported_clubs row
// and its own site as an events source, in one transaction. A club the
// lists named before is found by its key in imported_clubs (even after its
// admin renamed it); a new name goes to directory (ClubImports: it matches
// the university's clubs by name or opens an unclaimed one). Nothing of an
// existing club's profile changes.
func (s *service) importClub(ctx context.Context, src Source, c ClubCandidate) (bool, error) {
	name := clubDisplayName(c)
	key := directory.ClubNameKey(name)
	cat, review := clubCategory(c.SourceCategory, c.Name)
	social := map[string]string{}
	for k, v := range c.Social {
		if len(social) < 10 {
			social[k] = v
		}
	}
	email, err := s.clubEmail(ctx, src.UniversityID, c.Email)
	if err != nil {
		return false, err
	}
	created := false
	err = s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		var orgID uuid.UUID
		known, err := s.d.Store.FindImportedClub(ctx, src.UniversityID, key)
		switch {
		case err == nil:
			orgID = known.ClubOrgID
		case errors.Is(err, errNotFound):
			if orgID, created, err = s.d.Directory.ImportClub(ctx, directory.ImportedClub{
				UniversityID: src.UniversityID, Name: name, Category: cat, WebsiteURL: httpsSite(c.Website),
				SocialLinks: social, Job: fetchJob,
			}); err != nil {
				return err
			}
		default:
			return err
		}
		now := s.now()
		row := ImportedClub{
			ClubOrgID: orgID, UniversityID: src.UniversityID, SourceID: &src.ID, NameKey: key,
			NeedsReview: review && created, FirstSeenAt: now, LastSeenAt: now, ContactEmail: email,
		}
		if l := strings.TrimSpace(c.SourceCategory); l != "" {
			row.SourceCategory = ptr(clip(l, 100))
		}
		if err := s.d.Store.UpsertImportedClub(ctx, row); err != nil {
			return err
		}
		return s.clubSiteSource(ctx, src, orgID, name, c.Website)
	})
	return created, err
}

// clubSiteSource adds a club's own site as an events source (a social
// profile or a URL the guard would never fetch is not one). The source of a
// claimed club starts paused (claimed): the club speaks for itself.
func (s *service) clubSiteSource(ctx context.Context, src Source, orgID uuid.UUID, name, site string) error {
	u, host, ok := fetchableSite(site)
	if !ok {
		return nil // a social profile or a URL the guard never fetches is not a source
	}
	row := s.newSource(SourceInput{
		Kind: KindEvents, Adapter: AdapterHTMLPage, URL: u, Label: clip(name, 120), UniversityID: src.UniversityID,
		ClubOrgID: &orgID,
	}, host)
	club, err := s.d.Directory.GetClub(ctx, orgID)
	if err != nil {
		return err
	}
	if club.ClaimStatus == directory.ClaimClaimed {
		reason := PausedClaimed
		row.Enabled, row.PausedReason, row.NextRunAt = false, &reason, nil
	}
	_, _, err = s.d.Store.InsertSource(ctx, row)
	return err
}

// fetchableSite returns a club's site as a source URL and its host, or
// ok=false for a social profile or a URL the guard never fetches.
func fetchableSite(site string) (string, string, bool) {
	if site == "" || isSocial(site) {
		return "", "", false
	}
	u, host, err := ParseSourceURL(site)
	return u, host, err == nil
}

// clubEmail keeps a club's address only when its domain is the source
// university's own (a group address of the university, not a person's
// private mailbox).
func (s *service) clubEmail(ctx context.Context, universityID uuid.UUID, addr string) (*string, error) {
	domain, ok := emailDomain(addr)
	if !ok {
		return nil, nil
	}
	uni, found, err := s.d.Directory.UniversityByEmailDomain(ctx, domain)
	if err != nil {
		return nil, err
	}
	if !found || uni != universityID {
		return nil, nil
	}
	a := strings.ToLower(strings.TrimSpace(addr))
	return &a, nil
}

// newSource builds a source row with the module's defaults: the
// interval of its kind, enabled per the environment, due now when enabled.
func (s *service) newSource(in SourceInput, host string) Source {
	now := s.now()
	interval := in.Interval
	if interval == 0 {
		interval = s.d.Config.EventsInterval
		if in.Kind == KindClubList {
			interval = s.d.Config.ListInterval
		}
	}
	src := Source{
		ID: uuid.NewV7(), Kind: in.Kind, Adapter: in.Adapter, URL: in.URL, Host: host, Label: in.Label,
		UniversityID: in.UniversityID, ClubOrgID: in.ClubOrgID, IntervalSeconds: int(interval / time.Second),
		Enabled: s.d.Config.SourceDefaultEnabled,
	}
	if in.Enabled != nil {
		src.Enabled = *in.Enabled
	}
	if src.Enabled {
		src.NextRunAt = &now
	}
	return src
}

func ptr[T any](v T) *T { return &v }

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// clip cuts s to n runes.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }
