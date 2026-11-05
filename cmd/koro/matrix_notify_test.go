package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"time"

	"koro/internal/notify"
)

// matrixFixtures seeds fixtures/notify.yaml (03 §7.3) into the in-memory
// store installed by useMemStore: an in-app notification, a push token and a
// web push subscription of their owner persona, and one suppression under a
// fixed index.
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
		case "web_push_subscription":
			owner, err := fx.Ref(f, "owner")
			if err != nil {
				return err
			}
			endpoint := "https://fcm.googleapis.com/fcm/send/matrix-" + f.Name
			sum := sha256.Sum256([]byte(endpoint))
			_, err = w.store.UpsertWebPushSubscription(ctx, notify.WebPushSubscription{ID: id, UserID: owner, Endpoint: endpoint,
				EndpointSHA256: sum[:], P256DH: matrixP256DH(), Auth: make([]byte, notify.WebPushAuthLen), CreatedAt: now, LastSeenAt: now},
				notify.MaxWebPushSubscriptions)
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

// matrixP256DH is a browser public key for fixtures: a P-256 point made at
// run time (no key material in the repository).
func matrixP256DH() []byte {
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return k.PublicKey().Bytes()
}
