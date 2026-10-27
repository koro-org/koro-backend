package orders

import (
	"github.com/danielgtaylor/huma/v2"

	"koro/internal/platform/httpx/op"
)

// The status fields of the seller and delivery views are plain strings in
// Go; their OpenAPI enums come from the model's value lists (BE-117
// pattern, BE-127), so a client can switch on them exhaustively.

// TransformSchema implements huma.SchemaTransformer.
func (SellerLineDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	op.SetEnum(s, "seller_status", Statuses())
	op.SetEnum(s, "order_status", Statuses())
	return op.SetEnum(s, "fulfillment_status", Fulfillments())
}

// TransformSchema implements huma.SchemaTransformer.
func (LineStateDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	op.SetEnum(s, "order_status", Statuses())
	op.SetEnum(s, "seller_status", Statuses())
	op.SetEnum(s, "fulfillment_status", Fulfillments())
	return op.SetEnum(s, "fulfilled_via", FulfilledVias())
}

// TransformSchema implements huma.SchemaTransformer.
func (OrderCardDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	op.SetEnum(s, "status", Statuses())
	return op.SetEnum(s, "model_code", ModelCodes())
}

// TransformSchema implements huma.SchemaTransformer.
func (OrderDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	op.SetEnum(s, "status", Statuses())
	op.SetEnum(s, "seller_status", Statuses())
	return op.SetEnum(s, "model_code", ModelCodes())
}

// TransformSchema implements huma.SchemaTransformer.
func (OrderLineDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	op.SetEnum(s, "fulfillment_status", Fulfillments())
	return op.SetEnum(s, "fulfilled_via", FulfilledVias())
}

// TransformSchema implements huma.SchemaTransformer.
func (OrderQuoteDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	return op.SetEnum(s, "model_code", ModelCodes())
}
