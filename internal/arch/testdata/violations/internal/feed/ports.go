package feed

// ConsentRecorder: feed is not a consent caller in 01 §6.3 (05's "*" was narrowed).
type ConsentRecorder interface {
	// sync-write: consent.Recorder.Record
	Record(ctx context.Context) error
	// sync-write: entitlements.Entitlements.Consume
	Consume(ctx context.Context) error
}
