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
// The payment and refund actions (payments.payment.*, payments.refund.*) stay
// default deny until BE-043 binds their endpoints. Visibility: the admin
// surface, a collection account and a transfer are visible to every
// signed-in user (03 §7.2-3: 403 for an actor without the role, never 404).
// MFA follows from the role paths (k-mfa: pad, fin).
func paymentsPolicy() Policy {
	return Policy{
		Visible: func(a Actor, act Action, r Resource) bool {
			switch r.Type {
			case TypeAdminSurface:
				return AdminSurfaceVisible(a)
			case TypeCollectionAccount:
				return act == PaymentsCollectionAccountApprove && AdminSurfaceVisible(a)
			case TypeIncomingTransfer:
				return act == PaymentsTransferMarkReturned && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(_ Actor, act Action, r Resource) []Grant {
			pad, fin := Grant{Role: RolePlatformAdmin}, Grant{Role: RoleFinanceAdmin}
			switch r.Type {
			case TypeAdminSurface:
				switch act {
				case PaymentsCollectionAccountList, PaymentsCollectionAccountRetire, PaymentsTransferList, PaymentsTransferRead:
					return []Grant{pad, fin}
				case PaymentsCollectionAccountCreate, PaymentsTransferCreate, PaymentsTransferMatch, PaymentsTransferIgnore:
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
			}
			return nil
		},
	}
}
