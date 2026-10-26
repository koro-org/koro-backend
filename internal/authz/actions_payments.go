// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the payments module (03 §5.16), in table order.
const (
	PaymentsCollectionAccountList    Action = "payments.collection_account.list"
	PaymentsCollectionAccountCreate  Action = "payments.collection_account.create"
	PaymentsCollectionAccountApprove Action = "payments.collection_account.approve"
	PaymentsCollectionAccountRetire  Action = "payments.collection_account.retire"
	PaymentsTransferList             Action = "payments.transfer.list"
	PaymentsTransferRead             Action = "payments.transfer.read"
	PaymentsTransferCreate           Action = "payments.transfer.create"
	PaymentsTransferMatch            Action = "payments.transfer.match"
	PaymentsTransferIgnore           Action = "payments.transfer.ignore"
	PaymentsTransferMarkReturned     Action = "payments.transfer.mark_returned"
	PaymentsPaymentList              Action = "payments.payment.list"
	PaymentsPaymentRead              Action = "payments.payment.read"
	PaymentsRefundList               Action = "payments.refund.list"
	PaymentsRefundCreate             Action = "payments.refund.create"
	PaymentsRefundApprove            Action = "payments.refund.approve"
	PaymentsRefundReject             Action = "payments.refund.reject"
	PaymentsRefundComplete           Action = "payments.refund.complete"
	PaymentsRefundCancel             Action = "payments.refund.cancel"
	PaymentsRefundMarkFailed         Action = "payments.refund.mark_failed"
	PaymentsReceivableList           Action = "payments.receivable.list"
	PaymentsReceivableCreate         Action = "payments.receivable.create"
	PaymentsReceivableApprove        Action = "payments.receivable.approve"
	PaymentsReceivableReject         Action = "payments.receivable.reject"
	// PaymentsIbanReveal is the audited read of one full IBAN of a
	// collection account, an incoming transfer or a refund (BE-129).
	PaymentsIbanReveal Action = "payments.iban.reveal"
	// The refund requests of a buying company (BE-132, Q-26): the buyer
	// asks, finance decides; an accepted request opens a refund that keeps
	// its four eyes.
	PaymentsRefundRequestCreate       Action = "payments.refund_request.create"
	PaymentsRefundRequestList         Action = "payments.refund_request.list"
	PaymentsRefundRequestListSales    Action = "payments.refund_request.list_sales"
	PaymentsRefundRequestRead         Action = "payments.refund_request.read"
	PaymentsRefundRequestWithdraw     Action = "payments.refund_request.withdraw"
	PaymentsRefundRequestReadEvidence Action = "payments.refund_request.read_evidence"
	PaymentsRefundRequestListAdmin    Action = "payments.refund_request.list_admin"
	PaymentsRefundRequestReview       Action = "payments.refund_request.review"
	PaymentsRefundRequestAccept       Action = "payments.refund_request.accept"
	PaymentsRefundRequestReject       Action = "payments.refund_request.reject"
)

// paymentsActions lists the HTTP actions of payments in 03 §5.16 order.
var paymentsActions = []Action{
	PaymentsCollectionAccountList,
	PaymentsCollectionAccountCreate,
	PaymentsCollectionAccountApprove,
	PaymentsCollectionAccountRetire,
	PaymentsTransferList,
	PaymentsTransferRead,
	PaymentsTransferCreate,
	PaymentsTransferMatch,
	PaymentsTransferIgnore,
	PaymentsTransferMarkReturned,
	PaymentsPaymentList,
	PaymentsPaymentRead,
	PaymentsRefundList,
	PaymentsRefundCreate,
	PaymentsRefundApprove,
	PaymentsRefundReject,
	PaymentsRefundComplete,
	PaymentsRefundCancel,
	PaymentsRefundMarkFailed,
	PaymentsReceivableList,
	PaymentsReceivableCreate,
	PaymentsReceivableApprove,
	PaymentsReceivableReject,
	PaymentsIbanReveal,
	PaymentsRefundRequestCreate,
	PaymentsRefundRequestList,
	PaymentsRefundRequestListSales,
	PaymentsRefundRequestRead,
	PaymentsRefundRequestWithdraw,
	PaymentsRefundRequestReadEvidence,
	PaymentsRefundRequestListAdmin,
	PaymentsRefundRequestReview,
	PaymentsRefundRequestAccept,
	PaymentsRefundRequestReject,
}
