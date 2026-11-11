package ingest

import "koro/internal/platform/problem"

// Errors of the ingest module (05 §4.2).
var (
	// ErrNotFound: no such imported event, source or imported club (or not
	// visible: a hidden event on the public surface).
	ErrNotFound = problem.New(problem.NotFound, "ingest.not_found", "Not found")
	// ErrSourceExists: another source has this URL.
	ErrSourceExists = problem.New(problem.Conflict, "ingest.source_exists", "A source with this URL exists")
	// ErrURLNotAllowed: the URL is not a public http(s) address the
	// fetcher may read (scheme, port, credentials, private or local host).
	ErrURLNotAllowed = problem.New(problem.Invalid, "ingest.url_not_allowed", "URL is not allowed")
	// ErrAdapterMismatch: the adapter does not yield the source's kind, or
	// a club source names a club of another university.
	ErrAdapterMismatch = problem.New(problem.Invalid, "ingest.adapter_mismatch",
		"Adapter, kind and scope do not fit together")
	// ErrNotFetchable: an adapter that never fetches (instagram_graph,
	// manual_paste) cannot be enabled or run.
	ErrNotFetchable = problem.New(problem.Invalid, "ingest.not_fetchable", "This adapter does not fetch pages")
	// ErrInvalidState: the imported event is not in a state that allows
	// the change (already promoted, rejected, hidden …).
	ErrInvalidState = problem.New(problem.Conflict, "ingest.invalid_state", "Imported event state does not allow this")
	// ErrUnknownPilot: no built-in sources for this pilot key.
	ErrUnknownPilot = problem.New(problem.Invalid, "ingest.unknown_pilot", "Unknown pilot university")
)

// errUnavailable: a late-bound dependency is missing (503).
var errUnavailable = problem.ErrUnavailable
