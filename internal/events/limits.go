package events

import (
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/consent"
	"koro/internal/platform/httpx/op"
)

// Request field types whose OpenAPI bounds, patterns and enums come from
// the constants the service checks with (BE-117 pattern, BE-127): the
// client Zod schemas give early feedback with the very rule Go applies.
// The service still checks after normalising the text and keeps its own
// error codes.
type (
	// eventCancelReason is cancelEvent.cancel_reason (minCancelReason–maxCancelReason).
	eventCancelReason string
	// orgCancelReason is cancelEventRegistration.cancel_reason (minOrgCancel–maxRegCancel).
	orgCancelReason string
	// voidReasonText is voidCheckin.void_reason (minVoidReason–maxVoidReason).
	voidReasonText string
	// overrideReasonText is publishEventForm.lint_override_reason
	// (minOverrideReason–maxOverrideReason; omitted when the lint finds
	// nothing to override).
	overrideReasonText string
	// exportReasonText is createEventExport.reason (minExportReason–maxExportReason).
	exportReasonText string
	// exportField is one createEventExport.fields column: a base field
	// (ExportBaseFields) or answers.<form field key>.
	exportField string
	// exportFieldList is createEventExport.fields (1–maxExportFields
	// distinct columns).
	exportFieldList []exportField
)

// TransformSchema implements huma.SchemaTransformer.
func (exportFieldList) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	lo, hi := 1, maxExportFields
	s.MinItems, s.MaxItems, s.UniqueItems = &lo, &hi, true
	return s
}

// TransformSchema implements huma.SchemaTransformer.
func (eventCancelReason) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, minCancelReason, maxCancelReason)
}

// TransformSchema implements huma.SchemaTransformer.
func (orgCancelReason) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, minOrgCancel, maxRegCancel)
}

// TransformSchema implements huma.SchemaTransformer.
func (voidReasonText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, minVoidReason, maxVoidReason)
}

// TransformSchema implements huma.SchemaTransformer.
func (overrideReasonText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, minOverrideReason, maxOverrideReason)
}

// TransformSchema implements huma.SchemaTransformer.
func (exportReasonText) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.LengthBounds(s, minExportReason, maxExportReason)
}

// ExtExportBaseFields is the createEventExport.fields extension listing the
// base columns (the answers.<key> columns depend on the event's form).
const ExtExportBaseFields = "x-koro-export-base-fields"

// TransformSchema implements huma.SchemaTransformer: the allow-list is
// documented (x-koro-export-base-fields), not a schema pattern. The column
// rule stays the service's after authz (05 §5.3): a non-manager asking for
// a forbidden column (e-mail, T-69) gets the authz answer, never a
// validation error that would leak the rule before the permission check.
func (exportField) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Description = "A base field (" + ExtExportBaseFields + ") or " + AnswerFieldPrefix + "<form field key> of a " +
		"non-sensitive, non-consent field of the published form; anything else is 422 after authz"
	if s.Extensions == nil {
		s.Extensions = map[string]any{}
	}
	s.Extensions[ExtExportBaseFields] = ExportBaseFields()
	return s
}

// FormConsentPurposes lists the consent purposes a form's consent field may
// ask, sorted (formConsentPurposes).
func FormConsentPurposes() []consent.Purpose {
	out := make([]consent.Purpose, 0, len(formConsentPurposes))
	for p := range formConsentPurposes {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// TransformSchema implements huma.SchemaTransformer: consent_purpose lists
// the per-registration purposes the service accepts.
func (FormField) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.SetEnum(s, "consent_purpose", FormConsentPurposes())
}
