package talent

// Store is the persistence of the talent schema (02 §15). Every method
// runs inside a talent transaction: the service opens it with its
// TxManager and calls Enter first, which switches the transaction to the
// app_talent role (SET LOCAL ROLE app_talent; 01 §4.11, 02 §0.6). app_rw
// has no privilege on the schema, so a method called without Enter fails
// (errNoTalentTx in both stores; 42501 in PostgreSQL if it slipped
// through). Inside AsApp the transaction is back on the session role for
// public writes (the audit row) and the talent tables are unreachable.
//
// Talent tables have no org scope (02 §15: "Org kapsamı: yok"); every
// statement is keyed by the subject's user ID.
//
// Store is split by aggregate into embedded interfaces (05 §2.2, BE-146);
// a service file reaches the store only through the narrow view of the
// aggregate it works on (the accessors below), the retention sweeper
// through the views it holds (retention.go), the KVKK provider
// (privacy.go), which covers every table, through the whole Store.
type Store interface {
	roleStore           // Enter, AsApp and Leave: the role of the transaction (store_role.go)
	configStore         // signal definitions and config versions (store_config.go)
	counterStore        // daily counters, the caps (store_counters.go)
	ledgerStore         // point_events, the ledger (store_ledger.go)
	neutralizationStore // neutralizations (store_neutralizations.go)
	scoreStore          // the stored bands and scores (store_scores.go)
	scoringStore        // the nightly run: score runs, scores and cohorts (store_scoring.go)
	subjectStore        // every row of a data subject, KVKK (store_subject.go)
	retentionStore      // the purges of the retention sweeper (store_retention.go)
	reconcileStore      // the reconciliation of a closed period (store_reconcile.go)
}

// roles is the transaction role view of the store.
func (s *service) roles() roleStore { return s.d.Store }

// configs is the signal definitions and config versions view of the store.
func (s *service) configs() configStore { return s.d.Store }

// dailyCounters is the daily counters view of the store.
func (s *service) dailyCounters() counterStore { return s.d.Store }

// ledger is the point_events view of the store.
func (s *service) ledger() ledgerStore { return s.d.Store }

// neutralizations is the neutralisations view of the store.
func (s *service) neutralizations() neutralizationStore { return s.d.Store }

// scores is the stored bands and scores view of the store.
func (s *service) scores() scoreStore { return s.d.Store }

// scoring is the nightly run view of the store.
func (s *service) scoring() scoringStore { return s.d.Store }

// reconciliation is the closed period reconciliation view of the store.
func (s *service) reconciliation() reconcileStore { return s.d.Store }
