package authz

// paymentsPolicy is the policy of the payments module (03 §5.16). Only
// tickets of the payments module edit this file (BE-001 R5). Written by
// BE-041:
//
//	payments.collection_account.list     admin surface: pad, fin
//	payments.collection_account.create   admin surface: fin (R is the service's, 03 §6.2)
//	payments.collection_account.approve  on the account: pad·4, fin·4 (R); never its creator (the
//	                                     core four-eyes table). The service's "403 before reading"
//	                                     gate is collection_account.list on the admin surface (the
//	                                     same pad, fin cells), because the core demands created_by
//	                                     on every resource of this action.
//	payments.collection_account.retire   admin surface: pad, fin (R; retire and make-default)
//	payments.transfer.list / read        admin surface: pad, fin
//	payments.transfer.create             admin surface: fin (manual entry, CSV import)
//	payments.transfer.match              admin surface: fin (k27: the capture moves no money out; the
//	                                     overpayment refund it opens takes four eyes in BE-043). Also
//	                                     the model C direct confirmation (T-18; doc patch).
//	payments.transfer.ignore             admin surface: fin
//	payments.transfer.mark_returned      on the transfer: fin (R, k28; 06 H-18, H-56: request,
//	                                     second eye, receipt). The core four-eyes table refuses the
//	                                     requester when the service passes requested_by (the second
//	                                     eye's decision); the request and the receipt pass it empty.
//
// BE-043 adds the payments and the refunds (03 §5.16, §6.1 "İade"):
//
//	payments.payment.list / read         admin surface: pad, fin
//	payments.refund.list                 admin surface: pad, fin (the refunds and the refunds due)
//	payments.refund.create               admin surface: pad, fin (Idempotency-Key)
//	payments.refund.approve              on the refund: fin·4 (R is the service's); never its requester
//	                                     (the core four-eyes table demands requested_by). The
//	                                     service's "403 before reading" gate is payments.refund.list
//	                                     on the admin surface (ledger's payout approval pattern).
//	payments.refund.reject               admin surface: fin (the requester may withdraw it)
//	payments.refund.complete             admin surface: fin (any finance admin, with the receipt)
//
// BE-129 (no full IBAN in a list or detail response):
//
//	payments.iban.reveal                 admin surface: pad, fin (one full IBAN of a collection
//	                                     account, an incoming transfer or a refund; the access reason
//	                                     and the audit record are the service's)
//	payments.transfer.read               also the download of a transfer's bank receipt
//
// Visibility: the admin surface, a collection account, a transfer and a
// refund are visible to every signed-in user with a platform role path
// (03 §7.2-3: 403 for an actor without the role, never 404). MFA follows
// from the role paths (k-mfa: pad, fin).
func paymentsPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeReceivableCollection: {AttrStatus, AttrRequestedBy}},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeAdminSurface:
				return AdminSurfaceVisible(a)
			case TypeCollectionAccount:
				return act == PaymentsCollectionAccountApprove && AdminSurfaceVisible(a)
			case TypeIncomingTransfer:
				return act == PaymentsTransferMarkReturned && AdminSurfaceVisible(a)
			case TypeReceivableCollection:
				return act == PaymentsReceivableApprove && AdminSurfaceVisible(a)
			case TypeRefund:
				return (act == PaymentsRefundApprove || act == PaymentsRefundCancel || act == PaymentsRefundMarkFailed) && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(_ Actor, act Action, r Resource) []Grant {
			pad, fin := Grant{Role: RolePlatformAdmin}, Grant{Role: RoleFinanceAdmin}
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case PaymentsCollectionAccountList, PaymentsCollectionAccountRetire, PaymentsTransferList, PaymentsTransferRead,
					PaymentsPaymentList, PaymentsPaymentRead, PaymentsRefundList, PaymentsRefundCreate, PaymentsReceivableList,
					PaymentsIbanReveal:
					return []Grant{pad, fin}
				case PaymentsCollectionAccountCreate, PaymentsTransferCreate, PaymentsTransferMatch, PaymentsTransferIgnore,
					PaymentsRefundReject, PaymentsRefundComplete, PaymentsReceivableCreate, PaymentsReceivableReject:
					return []Grant{fin}
				}
			case TypeCollectionAccount:
				if act == PaymentsCollectionAccountApprove {
					return []Grant{pad, fin}
				}
			case TypeIncomingTransfer:
				if act == PaymentsTransferMarkReturned {
					return []Grant{fin}
				}
			case TypeReceivableCollection:
				if act == PaymentsReceivableApprove {
					return []Grant{fin}
				}
			case TypeRefund:
				if act == PaymentsRefundApprove || act == PaymentsRefundCancel || act == PaymentsRefundMarkFailed {
					// The eyes: the core table refuses the refund's requester.
					return []Grant{fin}
				}
			}
			return nil
		},
	}
}
