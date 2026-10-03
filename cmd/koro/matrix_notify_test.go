package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"koro/internal/notify"
)

// matrixFixtures seeds fixtures/notify.yaml (03 §7.3) into the in-memory
// store installed by useMemStore: an in-app notification and a push token of
// their owner persona, and one suppression under a fixed index.
func (w *notifyWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx, now := context.Background(), time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	for _, f := range fx.Of("notify") {
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		switch f.Kind {
		case "in_app":
			owner, err := fx.Ref(f, "owner")
			if err != nil {
				return err
			}
			_, err = w.store.InsertInApp(ctx, notify.InAppNotification{ID: id, UserID: owner, Kind: "notify.support_access",
				Title: "Matris", Body: "Matris fikstürü", LinkPath: "/me/security-events", CreatedAt: now})
			if err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "push_token":
			owner, err := fx.Ref(f, "owner")
			if err != nil {
				return err
			}
			sum := sha256.Sum256([]byte("matrix-token-" + f.Name))
			_, err = w.store.UpsertPushToken(ctx, notify.PushToken{ID: id, UserID: owner, Platform: notify.PushAndroid,
				Token: "matrix-token-" + f.Name, TokenSHA256: sum[:], CreatedAt: now, LastSeenAt: now})
			if err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		case "suppression":
			sum := sha256.Sum256([]byte("matrix-suppression"))
			if _, err := w.store.Suppress(ctx, notify.Suppression{EmailIndex: sum[:], Reason: notify.SuppressHardBounce,
				Source: notify.SourceProviderWebhook, CreatedAt: now}); err != nil {
				return fmt.Errorf("fixture %s: %w", f.Name, err)
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}
