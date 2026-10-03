package main

import (
	"context"
	"fmt"
	"time"

	"koro/internal/consent"
)

// matrixConsentText is the body of fixture notice_cc_email;
// bodies/consent/grant_commercial_email.json holds its SHA-256.
const matrixConsentText = "# Ticari elektronik ileti izni\n\nKampanya ve duyuruları e-posta ile almayı kabul ediyorum. (Matris fikstürü)\n"

// matrixFixtures seeds fixtures/consent.yaml (03 §7.3) into the in-memory
// store installed by useMemStore: notice versions in their lifecycle state.
func (w *consentWiring) matrixFixtures(fx *matrixFixtures) error {
	ctx, base := context.Background(), time.Now().UTC().Truncate(time.Microsecond)
	for _, f := range fx.Of("consent") {
		if f.Kind != "notice" {
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
		id, err := fx.ID(f.Name)
		if err != nil {
			return err
		}
		kind, _ := f.Fields["notice_kind"].(string)
		audience, _ := f.Fields["audience"].(string)
		version, _ := f.Fields["version"].(string)
		status, _ := f.Fields["status"].(string)
		body := "Matris fikstürü: " + f.Name + "\n"
		n := consent.Notice{
			ID: id, Kind: consent.NoticeKind(kind), Audience: consent.Audience(audience), Locale: consent.LocaleTR,
			Version: version, Title: "Matris " + f.Name, CreatedAt: base.Add(-48 * time.Hour),
		}
		if p, ok := f.Fields["purpose"].(string); ok {
			code := consent.Purpose(p)
			n.PurposeCode = &code
		}
		if f.Name == "notice_cc_email" {
			body = matrixConsentText
		}
		n.BodyMD, n.BodySHA256 = body, consent.BodyDigest(body)
		if err := w.store.InsertNotice(ctx, n); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
		switch status {
		case "draft":
		case "published", "retired":
			if err := w.store.Publish(ctx, id, base.Add(-24*time.Hour)); err != nil {
				return fmt.Errorf("fixture %s: publish: %w", f.Name, err)
			}
			if status == "retired" {
				if err := w.store.Retire(ctx, id, base.Add(-time.Hour)); err != nil {
					return fmt.Errorf("fixture %s: retire: %w", f.Name, err)
				}
			}
		default:
			return fmt.Errorf("fixture %s: unknown status %q", f.Name, status)
		}
	}
	return nil
}
