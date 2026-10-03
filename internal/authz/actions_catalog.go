// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the catalog module (03 §5.14), in table order.
const (
	CatalogServiceList              Action = "catalog.service.list"
	CatalogServiceManage            Action = "catalog.service.manage"
	CatalogOfferingBrowse           Action = "catalog.offering.browse"
	CatalogOfferingRead             Action = "catalog.offering.read"
	CatalogOfferingListOrg          Action = "catalog.offering.list_org"
	CatalogOfferingCreate           Action = "catalog.offering.create"
	CatalogOfferingUpdate           Action = "catalog.offering.update"
	CatalogOfferingSubmit           Action = "catalog.offering.submit"
	CatalogOfferingCancel           Action = "catalog.offering.cancel"
	CatalogOfferingReviewQueue      Action = "catalog.offering.review_queue"
	CatalogOfferingDecide           Action = "catalog.offering.decide"
	CatalogOfferingUniversityDecide Action = "catalog.offering.university_decide"
	CatalogPolicyList               Action = "catalog.policy.list"
	CatalogPolicyManage             Action = "catalog.policy.manage"
)

// catalogActions lists the HTTP actions of catalog in 03 §5.14 order.
var catalogActions = []Action{
	CatalogServiceList,
	CatalogServiceManage,
	CatalogOfferingBrowse,
	CatalogOfferingRead,
	CatalogOfferingListOrg,
	CatalogOfferingCreate,
	CatalogOfferingUpdate,
	CatalogOfferingSubmit,
	CatalogOfferingCancel,
	CatalogOfferingReviewQueue,
	CatalogOfferingDecide,
	CatalogOfferingUniversityDecide,
	CatalogPolicyList,
	CatalogPolicyManage,
}
