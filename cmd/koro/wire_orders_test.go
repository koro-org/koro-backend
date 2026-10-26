package main

import (
	"slices"
	"testing"
	"uuid"

	"koro/internal/orders"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// useMemStore installs the in-memory orders stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-040.
// Only tickets of the orders module edit this file (BE-001 R4).
func (w *ordersWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) orders.Store { return orders.NewMemStore(c.infra.clock.Now) }
}

// TestWire_Orders pins what the orders module provides and consumes: its
// service and the Orders contract, catalog's per-buyer count (BE-038's
// late binding), the closeOrg check, the KVKK provider, the subscriptions
// of 01 §6.2 (offering.cancelled on the critical queue, the notices), the
// dropped line's notice task (BE-134) and the jobs of 01 §7.
func TestWire_Orders(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.orders.svc == nil || c.orders.orders == nil {
		t.Fatal("orders service or Orders contract not provided")
	}
	if c.catalog.buyerHeld == nil {
		t.Error("catalog's per-buyer count (buyerHeld) is not bound")
	}
	if len(c.hooks.openCommerce) == 0 {
		t.Error("hooks.openCommerce has no orders check")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "orders" }) {
		t.Error("the orders KVKK provider is not registered")
	}
	for topic, name := range map[string]string{
		domainevent.TopicOfferingCancelled:          orders.CancelOfferingOrdersTaskName,
		domainevent.TopicOrderPlaced:                orders.NotifyOrderPlacedTaskName,
		domainevent.TopicOrderCancelled:             orders.NotifyOrderCancelledTaskName,
		domainevent.TopicOrderExpired:               orders.NotifyOrderExpiredTaskName,
		domainevent.TopicOrderLineFulfillmentMarked: orders.NotifyDeliveryMarkedTaskName,
		domainevent.TopicOrderLineFulfilled:         orders.NotifyLineFulfilledTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == name }) {
			t.Errorf("%s has no %s subscription", topic, name)
		}
	}
	if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(task queue.Task) bool { return task.Name == orders.NotifyLineDroppedTaskName }) {
		t.Errorf("task %s is not handled", orders.NotifyLineDroppedTaskName)
	}
	for name, cron := range map[string]string{orders.ExpireJobName: "50 * * * *", orders.CloseWindowsJobName: "55 * * * *"} {
		i := slices.IndexFunc(c.infra.jobs.Jobs(), func(j queue.Job) bool { return j.Name == name })
		if i < 0 {
			t.Errorf("job %s is not scheduled", name)
			continue
		}
		if j := c.infra.jobs.Jobs()[i]; j.Cron != cron || j.Queue != queue.Default {
			t.Errorf("job %s = %s on %s, want %s on default", name, j.Cron, j.Queue, cron)
		}
	}
	// The closeOrg check answers for an org without orders.
	open, err := c.hooks.openCommerce[len(c.hooks.openCommerce)-1](t.Context(), uuid.NewV7())
	if err != nil || open {
		t.Fatalf("openCommerce(no orders) = %v, %v", open, err)
	}
}
