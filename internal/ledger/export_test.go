package ledger

// TranslatePayout exposes the payout-flow error mapping to the store
// contract test: a deferred trigger (payout_items_sum) fails at COMMIT,
// where the service maps it the same way.
var TranslatePayout = translatePayout
