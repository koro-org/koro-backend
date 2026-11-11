// Action constants of the ingest module (BE-210; 03 §5.25 doc patch): the
// clubs and events collected from official pages (research
// kulup-etkinlik-toplama-2026-11-11, Aşama 1). TestActions_CountMatchesDesign
// and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the ingest module (03 §5.25), in table order.
const (
	IngestImportedEventList           Action = "ingest.imported_event.list"
	IngestImportedEventRead           Action = "ingest.imported_event.read"
	IngestImportedEventRequestRemoval Action = "ingest.imported_event.request_removal"
	IngestSuggestionList              Action = "ingest.suggestion.list"
	IngestSuggestionPromote           Action = "ingest.suggestion.promote"
	IngestSuggestionReject            Action = "ingest.suggestion.reject"
	IngestSourceList                  Action = "ingest.source.list"
	IngestSourceRead                  Action = "ingest.source.read"
	IngestSourceCreate                Action = "ingest.source.create"
	IngestSourceUpdate                Action = "ingest.source.update"
	IngestSourceRun                   Action = "ingest.source.run"
	IngestImportedEventListAdmin      Action = "ingest.imported_event.list_admin"
	IngestImportedEventModerate       Action = "ingest.imported_event.moderate"
	IngestImportedEventDelete         Action = "ingest.imported_event.delete"
	IngestImportedClubList            Action = "ingest.imported_club.list"
	IngestImportedClubReview          Action = "ingest.imported_club.review"
)

// ingestActions lists the HTTP actions of ingest in 03 §5.25 order.
var ingestActions = []Action{
	IngestImportedEventList, IngestImportedEventRead, IngestImportedEventRequestRemoval,
	IngestSuggestionList, IngestSuggestionPromote, IngestSuggestionReject,
	IngestSourceList, IngestSourceRead, IngestSourceCreate, IngestSourceUpdate, IngestSourceRun,
	IngestImportedEventListAdmin, IngestImportedEventModerate, IngestImportedEventDelete,
	IngestImportedClubList, IngestImportedClubReview,
}
