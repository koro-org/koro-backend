package consent_test

import (
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/privacy"
)

// TestSweeper_ConsentProof (consent_proof, 3 years after the withdrawal;
// BE-057): unapproved, the sweep counts; approved, the withdrawn chain goes
// with its İYS submission, a chain still granted stays.
func TestSweeper_ConsentProof(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	now := e.clk.Now()
	if got := e.mod.Sweeper.PolicyCodes(); !slices.Equal(got, []string{consent.PolicyConsentProof}) {
		t.Fatalf("PolicyCodes() = %v", got)
	}
	gone, live := uuid.NewV7(), uuid.NewV7()
	purpose := consent.PurposeCommercialEmail
	body := "Ticari ileti metni"
	text := consent.Notice{ID: uuid.NewV7(), Kind: consent.KindConsentText, PurposeCode: &purpose, Audience: consent.AudienceAll,
		Locale: consent.LocaleTR, Version: "2021.01.01", Title: "Başlık", BodyMD: body, BodySHA256: consent.BodyDigest(body), CreatedAt: now}
	if err := e.store.InsertNotice(ctx, text); err != nil {
		t.Fatal(err)
	}
	add := func(u uuid.UUID, a consent.Action, at time.Time) consent.Consent {
		t.Helper()
		c := consent.Consent{ID: uuid.NewV7(), UserID: u, PurposeCode: purpose, Action: a,
			ScopeType: consent.ScopeNone, Channel: consent.ChannelWeb, OccurredAt: at}
		if a == consent.Granted {
			c.NoticeVersionID, c.TextSHA256 = &text.ID, text.BodySHA256
		}
		if err := e.store.InsertConsent(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	g := add(gone, consent.Granted, now.AddDate(-5, 0, 0))
	add(gone, consent.Withdrawn, now.AddDate(-4, 0, 0))
	if err := e.store.InsertSubmission(ctx, consent.Submission{ID: uuid.NewV7(), ConsentID: g.ID, Channel: consent.IYSEmail,
		Status: consent.SubmissionPending}); err != nil {
		t.Fatal(err)
	}
	add(live, consent.Granted, now.AddDate(-5, 0, 0))
	p := privacy.RetentionPolicy{Code: consent.PolicyConsentProof, Retention: now.Sub(now.AddDate(-3, 0, 0))}
	if res, err := e.mod.Sweeper.Sweep(ctx, p, now); err != nil || res.Count != 2 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	if rows, _ := e.store.AllOf(ctx, gone); len(rows) != 2 {
		t.Fatalf("a dry run deleted consents: %d left", len(rows))
	}
	p.LegalApproved = true
	if res, err := e.mod.Sweeper.Sweep(ctx, p, now); err != nil || res.Count != 2 || res.Table != "consents" {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if rows, _ := e.store.AllOf(ctx, gone); len(rows) != 0 {
		t.Fatalf("withdrawn chain left: %d rows", len(rows))
	}
	if _, err := e.store.SubmissionOf(ctx, g.ID); err == nil {
		t.Fatal("the İYS submission of the purged consent survived")
	}
	if rows, _ := e.store.AllOf(ctx, live); len(rows) != 1 {
		t.Fatalf("the granted chain was purged: %d rows", len(rows))
	}
}
