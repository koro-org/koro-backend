package events

type gormStore struct{}

// Get is scoped.
func (s *gormStore) Get(ctx context.Context, orgID, id uuid.UUID) (Event, error) {
	return Event{}, db.Conn(ctx, s.db).Scopes(db.ScopeOrg(orgID)).Take(&Event{}).Error
}

// List forgets the org scope.
func (s *gormStore) List(ctx context.Context, orgID uuid.UUID) ([]Event, error) {
	var out []Event
	return out, db.Conn(ctx, s.db).Find(&out).Error
}

// Count reads a table without org_id.
// cross-org: universities are global reference data
func (s *gormStore) Count(ctx context.Context) (int64, error) {
	return 0, nil
}

// Raw uses named-parameter SQL.
func (s *gormStore) Raw(ctx context.Context, orgID uuid.UUID) error {
	return db.Conn(ctx, s.db).Exec("DELETE FROM events WHERE org_id = @org_id", sql.Named("org_id", orgID)).Error
}

// Helper uses the scoped helper.
func (s *gormStore) Helper(ctx context.Context, orgID uuid.UUID) error {
	return s.scoped(ctx, orgID).Find(&[]Event{}).Error
}
