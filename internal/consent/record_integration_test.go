//go:build integration

package consent_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// TestRecord_Gorm_NonIYSPurposeEventInSameTx (BE-104, T-355): over
// PostgreSQL a contact_info_share withdrawal writes its consent.recorded
// outbox row in the recording transaction (a caller's rollback removes
// it with the consent) and no İYS submission; consent.submit_iys passes
// the committed event without error and leaves iys_submissions empty.
func TestRecord_Gorm_NonIYSPurposeEventInSameTx(t *testing.T) {
	t.Parallel()
	e := newPGEnv(t)
	ctx := t.Context()
	user := insertUser(t, e.d, uuid.NewV7())
	text := e.consentText(t, consent.PurposeContactInfoShare)
	if _, err := e.svc.Record(ctx, grant(user, text, consent.ChannelWeb)); err != nil {
		t.Fatal(err)
	}
	events := func() int {
		return e.count(t, `SELECT count(*) FROM outbox WHERE topic = $1 AND payload->>'user_id' = $2`,
			domainevent.TopicConsentRecorded, user.String())
	}
	if n := events(); n != 1 {
		t.Fatalf("consent.recorded of the grant = %d, want 1", n)
	}

	err := e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if _, err := e.svc.Record(ctx, withdraw(user, consent.PurposeContactInfoShare)); err != nil {
			return err
		}
		return errBoom // the caller fails after the consent step
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("WithTx = %v", err)
	}
	if n := events(); n != 1 {
		t.Fatalf("consent.recorded after the rollback = %d, want 1", n)
	}
	if on, err := e.svc.HasActive(ctx, user, consent.PurposeContactInfoShare, nil); err != nil || !on {
		t.Fatalf("HasActive after the rolled back withdrawal = %v, %v", on, err)
	}

	e.clk.Advance(time.Second)
	id, err := e.svc.Record(ctx, withdraw(user, consent.PurposeContactInfoShare))
	if err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := e.d.Pool.QueryRow(ctx, `SELECT payload FROM outbox WHERE topic = $1 AND aggregate_id = $2`,
		domainevent.TopicConsentRecorded, id).Scan(&payload); err != nil {
		t.Fatalf("the committed withdrawal has no consent.recorded row: %v", err)
	}
	want := domainevent.ConsentRecorded{ConsentID: id, UserID: user,
		Purpose: string(consent.PurposeContactInfoShare), Action: string(consent.Withdrawn)}
	var got domainevent.ConsentRecorded
	if err := json.Unmarshal(payload, &got); err != nil || got != want {
		t.Fatalf("stored payload %s (%v), want %+v", payload, err, want)
	}
	ev, err := domainevent.New(want, "consent", id)
	if err != nil {
		t.Fatal(err)
	}
	r := outbox.NewRegistry()
	e.mod.Subscribe(r)
	if err := r.For(consent.TopicRecorded)[0].Handle(ctx, ev); err != nil {
		t.Fatalf("consent.submit_iys of a contact_info_share withdrawal = %v, want a no-op", err)
	}
	if n := e.count(t, `SELECT count(*) FROM iys_submissions s JOIN consents c ON c.id = s.consent_id WHERE c.user_id = $1`, user); n != 0 {
		t.Fatalf("İYS submissions of a purpose without İYS = %d", n)
	}
	if len(e.iys.Sent()) != 0 {
		t.Fatalf("İYS received %+v", e.iys.Sent())
	}
}
