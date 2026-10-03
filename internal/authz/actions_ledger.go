// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the ledger module (03 §5.17), in table order.
const (
	LedgerBalanceRead      Action = "ledger.balance.read"
	LedgerJournalList      Action = "ledger.journal.list"
	LedgerPayableList      Action = "ledger.payable.list"
	LedgerPayoutList       Action = "ledger.payout.list"
	LedgerPayoutRead       Action = "ledger.payout.read"
	LedgerPayoutCreate     Action = "ledger.payout.create"
	LedgerPayoutApprove    Action = "ledger.payout.approve"
	LedgerPayoutMarkPaid   Action = "ledger.payout.mark_paid"
	LedgerPayoutMarkFailed Action = "ledger.payout.mark_failed"
	LedgerPayoutCancel     Action = "ledger.payout.cancel"
)

// ledgerActions lists the HTTP actions of ledger in 03 §5.17 order.
var ledgerActions = []Action{
	LedgerBalanceRead,
	LedgerJournalList,
	LedgerPayableList,
	LedgerPayoutList,
	LedgerPayoutRead,
	LedgerPayoutCreate,
	LedgerPayoutApprove,
	LedgerPayoutMarkPaid,
	LedgerPayoutMarkFailed,
	LedgerPayoutCancel,
}
