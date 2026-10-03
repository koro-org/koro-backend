// Package events is a miniature module that breaks every architecture rule
// (internal/arch tests). It is parsed, never compiled.
package events

import (
	"koro/internal/audit"
	"koro/internal/orders"
	"koro/internal/students"
	st "koro/internal/students"
	"koro/internal/talent"
)

type service struct {
	store  st.Store      // foreign Store (05 §2.4-1)
	orders orders.Orders // higher layer (01 §5.1)
	band   talent.Band   // talent ban (01 §5.4)
	audit  audit.Logger  // lower layer: fine
}

func newService() *service {
	_ = students.NewMemStore() // foreign NewMemStore through the package name
	return &service{}
}
