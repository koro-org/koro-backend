//go:build integration

package students_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
	"koro/internal/students"
)

// TestStore_GrantContacts_Gorm: the per-company sharing store calls on
// PostgreSQL (migration 000136, profile_access_grants_contact_shared_chk).
func TestStore_GrantContacts_Gorm(t *testing.T) {
	t.Parallel()
	d := testutil.NewDB(t)
	grantContactsContract(t, students.NewGormStore(d.ORM), pgWorld(d))
}

// TestShareContactWithCompany_ConsentFailureRollsBack_Postgres (T-345, 01
// §6.3; BE-089): over PostgreSQL and the real consent module, inside the
// caller's transaction (messaging's acceptance): a text that is not the
// current contact_info_share text (422 consent.text_mismatch) leaves no
// share, no consent row and no setting; a caller's transaction that fails
// after the sharing takes the share and the consent back; the current
// text shares with the grant's company alone, the consent in force.
func TestShareContactWithCompany_ConsentFailureRollsBack_Postgres(t *testing.T) {
	t.Parallel()
	e := newPgEnv(t)
	ctx := t.Context()
	a := e.pgStudent(t)
	orgA, orgB := pgWorld(e.d).company(t), pgWorld(e.d).company(t)
	for _, org := range []uuid.UUID{orgA, orgB} {
		if err := e.svc.Grant(ctx, a.UserID, org, students.SourceContactRequest, uuid.NewV7()); err != nil {
			t.Fatal(err)
		}
	}
	body := "# İletişim\n\nİletişim talebini kabul ettiğim şirketin e-posta adresimi görmesine izin veriyorum.\n"
	purpose := consent.PurposeContactInfoShare
	n := consent.Notice{ID: uuid.NewV7(), Kind: consent.KindConsentText, PurposeCode: &purpose, Audience: consent.Audience("student"),
		Locale: consent.LocaleTR, Version: "2026.10.23", Title: "İletişim paylaşımı rızası", BodyMD: body,
		BodySHA256: consent.BodyDigest(body), CreatedAt: t0.Add(-time.Hour)}
	cstore := consent.NewGormStore(e.d.ORM)
	if err := cstore.InsertNotice(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := cstore.Publish(ctx, n.ID, t0.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	count := func(sql string) int {
		t.Helper()
		var c int
		if err := e.d.Pool.QueryRow(ctx, sql, a.UserID).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	consents := func() int {
		return count(`SELECT count(*) FROM consents WHERE user_id = $1 AND purpose_code = 'contact_info_share'`)
	}
	shares := func() int {
		return count(`SELECT count(*) FROM profile_access_grants WHERE student_user_id = $1 AND contact_shared_at IS NOT NULL`)
	}
	shown := students.ConsentProof{NoticeVersionID: n.ID, TextSHA256: n.BodySHA256}
	wrong := students.ConsentProof{NoticeVersionID: n.ID, TextSHA256: make([]byte, 32)}
	gs := students.NewGormStore(e.d.ORM)

	err := e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return e.svc.ShareContactWithCompany(ctx, a, orgA, wrong, consent.ChannelWeb, uuid.NewV7())
	})
	wantCode(t, err, "consent.text_mismatch")
	if _, err := gs.GetPrivacy(ctx, a.UserID); !errors.Is(err, students.ErrNotFound) || consents() != 0 || shares() != 0 {
		t.Fatalf("a refused sharing wrote: settings %v, consents %d, shares %d", err, consents(), shares())
	}

	boom := errors.New("acceptance failed later")
	err = e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := e.svc.ShareContactWithCompany(ctx, a, orgA, shown, consent.ChannelWeb, uuid.NewV7()); err != nil {
			return err
		}
		if !db.InTx(ctx) {
			return errors.New("not in a transaction")
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("rolled back sharing = %v", err)
	}
	if consents() != 0 || shares() != 0 {
		t.Fatalf("the sharing outlived its transaction: consents %d, shares %d", consents(), shares())
	}

	if err := e.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		return e.svc.ShareContactWithCompany(ctx, a, orgA, shown, consent.ChannelWeb, uuid.NewV7())
	}); err != nil {
		t.Fatal(err)
	}
	if consents() != 1 || shares() != 1 ||
		count(`SELECT count(*) FROM profile_access_grants WHERE student_user_id = $1 AND contact_shared_at IS NOT NULL
			AND company_org_id = '`+orgA.String()+`'`) != 1 {
		t.Fatalf("shared: consents %d, shares %d (company A alone)", consents(), shares())
	}
	if ok, err := e.consent.HasActive(ctx, a.UserID, consent.PurposeContactInfoShare, nil); err != nil || !ok {
		t.Fatalf("consent not in force: %v %v", ok, err)
	}
	if p, err := gs.GetPrivacy(ctx, a.UserID); err != nil || !p.ShowContactOnGrant {
		t.Fatalf("setting = %+v, %v (mirrors the consent)", p, err)
	}
}
