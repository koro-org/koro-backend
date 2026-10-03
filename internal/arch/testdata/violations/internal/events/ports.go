package events

// OrderApplier is not on the 01 §6.3 list for events.
type OrderApplier interface {
	// sync-write: orders.Orders.ApplyPayment
	ApplyPayment(ctx context.Context, orderID uuid.UUID) error
}

// Things writes without declaring it.
type Things interface {
	CreateThing(ctx context.Context) error
	GetThing(ctx context.Context) error
}

// Allowed ports.
type Allowed interface {
	// sync-write: audit.Logger.Log
	Log(ctx context.Context) error
	// sync-write: consent.Recorder.Record
	Record(ctx context.Context) error
}
