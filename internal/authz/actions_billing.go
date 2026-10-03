// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the billing module (03 §5.18), in table order.
const (
	BillingInvoiceList       Action = "billing.invoice.list"
	BillingInvoiceRead       Action = "billing.invoice.read"
	BillingInvoiceUpload     Action = "billing.invoice.upload"
	BillingInvoiceVerify     Action = "billing.invoice.verify"
	BillingInvoiceCancel     Action = "billing.invoice.cancel"
	BillingInvoiceListAdmin  Action = "billing.invoice.list_admin"
	BillingIntegrationManage Action = "billing.integration.manage"
)

// System-only actions of the billing module (03 §3.7): only SystemActor may perform them.
const (
	BillingInvoicePlan Action = "billing.invoice.plan"
)

// billingActions lists the HTTP actions of billing in 03 §5.18 order.
var billingActions = []Action{
	BillingInvoiceList,
	BillingInvoiceRead,
	BillingInvoiceUpload,
	BillingInvoiceVerify,
	BillingInvoiceCancel,
	BillingInvoiceListAdmin,
	BillingIntegrationManage,
}
