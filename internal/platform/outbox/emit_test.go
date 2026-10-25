package outbox_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/platform/clock"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

func TestEmit_BuildsEnvelopeOnAggregate(t *testing.T) {
	t.Parallel()
	mem := outbox.NewMem(clock.NewFake(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)))
	id := uuid.NewV7()
	o := domainevent.OrgInvitationCreated{InvitationID: id, OrgID: uuid.NewV7()}
	a := domainevent.MembershipStarted{MembershipID: id, OrgID: uuid.NewV7(), UserID: uuid.NewV7()}
	err := db.NopTx{}.WithTx(t.Context(), func(ctx context.Context) error {
		if err := outbox.Emit(ctx, mem, o, "org_invitation", id); err != nil {
			return err
		}
		return outbox.EmitAfterCommit(ctx, mem, a, "org_member", id)
	})
	if err != nil {
		t.Fatal(err)
	}
	ev := mem.Events()
	if len(ev) != 2 || ev[0].Topic != o.Topic() || ev[0].AggregateType != "org_invitation" || ev[0].AggregateID != id ||
		ev[1].Topic != a.Topic() || ev[1].AggregateType != "org_member" {
		t.Fatalf("events = %+v", ev)
	}
	if err := outbox.Emit(t.Context(), mem, o, "org_invitation", id); !errors.Is(err, outbox.ErrNoTx) {
		t.Fatalf("Emit outside a tx = %v, want ErrNoTx", err)
	}
	if err := outbox.EmitAfterCommit(t.Context(), mem, o, "org_invitation", id); !errors.Is(err, outbox.ErrWrongMechanism) {
		t.Fatalf("EmitAfterCommit of an O topic = %v, want ErrWrongMechanism", err)
	}
}
