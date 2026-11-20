package ingest

import (
	"context"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/events"
	"koro/internal/files"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/ratelimit"
)

// The ports of ingest (05 §1.3: consumer side, narrow). External systems
// (the web, the language model) are a port with adapters chosen by
// configuration (INGEST_EXTRACTOR_PROVIDER); the modules below ingest are
// their services, passed by cmd/koro/wire_ingest.go.

// Fetcher reads the web for the pipeline: robots.txt first, then the page
// with a conditional GET (fetch.go: HTTPFetcher, the SSRF guard, the size
// and time limits, the User-Agent). Tests use an in-memory fetcher.
type Fetcher interface {
	// Robots tells whether the User-Agent may fetch rawURL and the
	// host's Crawl-delay (0 when none); a missing robots.txt allows all.
	Robots(ctx context.Context, rawURL string) (RobotsVerdict, error)
	// Fetch reads rawURL. A 304 answer is NotModified; a non-2xx status is
	// a Page with that Status and no body.
	Fetch(ctx context.Context, req FetchRequest) (Page, error)
}

// RobotsVerdict is the robots.txt answer for one URL.
type RobotsVerdict struct {
	Allowed    bool
	CrawlDelay time.Duration
	// Unreachable: robots.txt answered 5xx or not at all; the URL is not
	// fetched this time (RFC 9309 §2.3.1.4) but nothing is decided.
	Unreachable bool
}

// FetchRequest is one conditional GET.
type FetchRequest struct {
	URL          string
	ETag         string
	LastModified string
	// Accept lists the content types the adapter reads (media types
	// without parameters); another answer is ErrContentType.
	Accept []string
}

// Page is the answer of a fetch (untrusted data: never an instruction).
type Page struct {
	URL          string // the final URL after redirects
	Status       int
	NotModified  bool
	ContentType  string // media type, lower case, no parameters
	Body         []byte
	ETag         string
	LastModified string
}

// Extractor is the language model (extract.go, extract_draft.go: the
// requests and the validation; extractor_fake.go, extractor_claude.go).
// Its input is untrusted data: an adapter never follows an instruction
// found in it and has no tool. One port with both use cases, so one
// INGEST_EXTRACTOR_PROVIDER selection serves the pipeline and the quick
// import alike.
type Extractor interface {
	// Extract turns the masked text of pages into event candidates (the
	// pipeline, BE-210).
	Extract(ctx context.Context, req ExtractRequest) (Extraction, error)
	// Draft reads a club's own post (its masked text and/or its poster
	// images) into draft event fields with a confidence per field (the
	// quick import, BE-211). An adapter that cannot read images says so
	// (DraftExtraction.ImagesRead false).
	Draft(ctx context.Context, req DraftRequest) (DraftExtraction, error)
	// Name is the adapter name (metrics, run logs).
	Name() string
}

// Files is what the quick import needs of files (L2; 01 §4.2): the
// metadata of a club's poster upload and, after the checks, its bytes. The
// uploads are files' ordinary event_cover uploads (createUpload +
// completeUpload + the scan), so a poster the club then uses as its event's
// cover is the same file, and one it never uses goes with files'
// orphan cleanup: ingest keeps no image.
type Files interface {
	Get(ctx context.Context, fileID uuid.UUID) (files.FileMeta, error)
	Read(ctx context.Context, fileID uuid.UUID, maxBytes int64) ([]byte, files.FileMeta, error)
}

// Limiter is the platform's rate limiter (Redis in production): the quick
// import's per-club limits and its platform-wide daily budget (the
// language model's cost).
type Limiter interface {
	Allow(ctx context.Context, rule ratelimit.Rule, key string) (ratelimit.Result, error)
}

// Directory is what ingest needs of directory (L6): the university of a
// source, the facts of a club, the university of a club's e-mail domain
// and the import of the clubs of an official list.
type Directory interface {
	GetUniversity(ctx context.Context, id uuid.UUID) (directory.University, error)
	GetClub(ctx context.Context, orgID uuid.UUID) (directory.Club, error)
	UniversityByEmailDomain(ctx context.Context, domain string) (uuid.UUID, bool, error)
	// ImportClub returns the club of a list entry, opening an unclaimed one
	// when the university has none of that name, in the caller's
	// transaction (ingest writes its imported_clubs row in the same one).
	//
	// sync-write: directory.ClubImports.ImportClub
	ImportClub(ctx context.Context, in directory.ImportedClub) (uuid.UUID, bool, error)
}

// Drafts makes an imported event an ordinary draft event of its club
// (events, L8) when the club's admin promotes it: events authorises the
// actor itself (events.event.create) and the draft is created in the
// promotion's transaction, so an imported event is never promoted without
// its draft nor a draft left behind by a failed promotion.
type Drafts interface {
	// sync-write: events.Service.CreateEvent
	CreateEvent(ctx context.Context, a authz.Actor, orgID uuid.UUID, in events.EventInput) (events.EventView, error)
}

// Sender is notify's Sender (L3): the in-app notice to a club's new admin
// that suggestions wait (club_claim.decided, approved).
type Sender interface {
	Send(ctx context.Context, m notify.Message) (uuid.UUID, error)
}

// AuditLogger is audit.Logger (every admin write, same transaction).
type AuditLogger interface {
	// sync-write: audit.Logger.Log
	Log(ctx context.Context, e audit.Event) error
}

// Orgs is orgs' org directory (L5; 01 §4.7 OrgDirectory): the club of a
// suggestion (authorisation: kind and status), the club names of the
// event cards and the slug of the new admin's notice.
type Orgs interface {
	Get(ctx context.Context, orgID uuid.UUID) (orgs.OrgSummary, error)
	GetMany(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]orgs.OrgSummary, error)
}
