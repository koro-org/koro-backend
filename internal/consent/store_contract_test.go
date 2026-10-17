package consent_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/db"
	"koro/internal/platform/problem"
)

// storeWorld is one fresh store with its transaction manager; user creates
// a row the consents/notice FKs may point to (users belong to identity).
type storeWorld struct {
	s    consent.Store
	tx   db.TxManager
	user func(t *testing.T) uuid.UUID
}

// storeContract is the contract both stores pass (05 §13.3): the in-memory
// one here, the GORM one against PostgreSQL in store_integration_test.go.
func storeContract(t *testing.T, newWorld func(t *testing.T) storeWorld) {
	t.Helper()
	base := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	email := consent.PurposeCommercialEmail

	notice := func(kind consent.NoticeKind, purpose *consent.Purpose, aud consent.Audience, version string) consent.Notice {
		body := "Metin " + version + " " + string(kind)
		return consent.Notice{ID: uuid.NewV7(), Kind: kind, PurposeCode: purpose, Audience: aud, Locale: consent.LocaleTR,
			Version: version, Title: "Başlık", BodyMD: body, BodySHA256: consent.BodyDigest(body), CreatedAt: base}
	}

	t.Run("purposes are the 000011 seed", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		got, err := w.s.Purposes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var codes []consent.Purpose
		for _, p := range got {
			codes = append(codes, p.Code)
		}
		want := []consent.Purpose{"activity_ranking", "commercial_email", "commercial_push", "commercial_sms",
			"contact_info_share", "discovery_profile", "event_sponsor_share"}
		if !slices.Equal(codes, want) {
			t.Fatalf("purposes = %v, want %v", codes, want)
		}
		p, err := w.s.Purpose(ctx, email)
		if err != nil || !p.RequiresIYS || p.IYSChannel == nil || *p.IYSChannel != consent.IYSEmail ||
			p.LegalBasis != consent.BasisExplicitConsent || p.ScopeType != consent.ScopeNone || !p.Active || p.DescriptionTR == "" {
			t.Fatalf("commercial_email = %+v, %v", p, err)
		}
		if p, _ := w.s.Purpose(ctx, consent.PurposeEventSponsor); p.ScopeType != consent.ScopeRegistration || p.RequiresIYS {
			t.Fatalf("event_sponsor_share = %+v", p)
		}
		if p, _ := w.s.Purpose(ctx, consent.PurposeActivityRanking); p.Active {
			t.Fatal("activity_ranking must start passive (T-32)")
		}
		if _, err := w.s.Purpose(ctx, "selfie_share"); !errors.Is(err, consent.ErrUnknownPurpose) {
			t.Fatalf("unknown purpose = %v", err)
		}
		err = w.tx.WithTx(ctx, func(ctx context.Context) error {
			p, err := w.s.LockPurpose(ctx, consent.PurposeCommercialPush)
			if err != nil {
				return err
			}
			p.Active, p.DescriptionTR = false, "Yeni açıklama"
			return w.s.UpdatePurpose(ctx, p)
		})
		if err != nil {
			t.Fatal(err)
		}
		if p, _ := w.s.Purpose(ctx, consent.PurposeCommercialPush); p.Active || p.DescriptionTR != "Yeni açıklama" || p.RequiresIYS {
			t.Fatalf("after update = %+v", p)
		}
		if err := w.s.UpdatePurpose(ctx, consent.PurposeDef{Code: "selfie_share"}); !errors.Is(err, consent.ErrUnknownPurpose) {
			t.Fatalf("update of an unknown purpose = %v", err)
		}
	})

	t.Run("notice lifecycle", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		dpo := w.user(t)
		v1 := notice(consent.KindConsentText, &email, consent.AudienceAll, "2026.10.01")
		v1.CreatedBy = &dpo
		if err := w.s.InsertNotice(ctx, v1); err != nil {
			t.Fatal(err)
		}
		got, err := w.s.Notice(ctx, v1.ID)
		if err != nil || got.Status() != consent.StatusDraft || !bytes.Equal(got.BodySHA256, v1.BodySHA256) ||
			*got.PurposeCode != email || *got.CreatedBy != dpo || got.BodyMD != v1.BodyMD || !got.CreatedAt.Equal(base) {
			t.Fatalf("Notice = %+v, %v", got, err)
		}
		if _, err := w.s.Notice(ctx, uuid.NewV7()); !errors.Is(err, consent.ErrNotFound) {
			t.Fatalf("missing notice = %v", err)
		}
		dup := notice(consent.KindConsentText, &email, consent.AudienceAll, "2026.10.01")
		if err := w.s.InsertNotice(ctx, dup); !errors.Is(err, consent.ErrNoticeVersionTaken) {
			t.Fatalf("duplicate version = %v", err)
		}
		bad := []consent.Notice{
			notice(consent.KindConsentText, nil, consent.AudienceAll, "2026.10.02"),          // consent text without purpose
			notice(consent.KindPrivacyNotice, &email, consent.AudienceAll, "2026.10.02"),     // privacy notice with purpose
			notice(consent.KindPrivacyNotice, nil, consent.AudienceAll, "v2"),                // version format
			notice(consent.KindPrivacyNotice, nil, consent.Audience("guests"), "2026.10.02"), // audience
		}
		for i, n := range bad {
			if err := w.s.InsertNotice(ctx, n); !errors.Is(err, problem.ErrValidation) {
				t.Fatalf("bad notice %d = %v, want 422", i, err)
			}
		}
		unknown := consent.Purpose("selfie_share")
		if err := w.s.InsertNotice(ctx, notice(consent.KindConsentText, &unknown, consent.AudienceAll, "2026.10.02")); !errors.Is(err, consent.ErrUnknownPurpose) {
			t.Fatalf("unknown purpose = %v", err)
		}

		// Draft → published → retired; a second version takes the key.
		if _, found, _ := w.s.LockCurrent(ctx, v1.Key()); found {
			t.Fatal("a draft is current")
		}
		if err := w.s.Retire(ctx, v1.ID, base); !errors.Is(err, consent.ErrNoticeState) {
			t.Fatalf("retire of a draft = %v", err)
		}
		if err := w.s.Publish(ctx, v1.ID, base.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := w.s.Publish(ctx, v1.ID, base.Add(time.Hour)); !errors.Is(err, consent.ErrNoticeState) {
			t.Fatalf("second publish = %v", err)
		}
		v2 := notice(consent.KindConsentText, &email, consent.AudienceAll, "2026.10.02")
		if err := w.s.InsertNotice(ctx, v2); err != nil {
			t.Fatal(err)
		}
		if err := w.s.Publish(ctx, v2.ID, base.Add(2*time.Hour)); !errors.Is(err, consent.ErrNoticeState) {
			t.Fatalf("publish while another version is current = %v, want notice_versions_current_uq", err)
		}
		err = w.tx.WithTx(ctx, func(ctx context.Context) error {
			cur, found, err := w.s.LockCurrent(ctx, v2.Key())
			if err != nil || !found || cur.ID != v1.ID {
				t.Fatalf("LockCurrent = %v %v %v", cur.ID, found, err)
			}
			if err := w.s.Retire(ctx, cur.ID, base.Add(2*time.Hour)); err != nil {
				return err
			}
			return w.s.Publish(ctx, v2.ID, base.Add(2*time.Hour))
		})
		if err != nil {
			t.Fatal(err)
		}
		old, _ := w.s.Notice(ctx, v1.ID)
		if old.Status() != consent.StatusRetired || !old.RetiredAt.Equal(base.Add(2*time.Hour)) || !old.PublishedAt.Equal(base.Add(time.Hour)) {
			t.Fatalf("v1 = %+v", old)
		}
		if err := w.s.Retire(ctx, v1.ID, base.Add(3*time.Hour)); !errors.Is(err, consent.ErrNoticeState) {
			t.Fatalf("second retire = %v", err)
		}

		// CurrentNotices filters and orders by kind, purpose, audience, locale.
		priv := notice(consent.KindPrivacyNotice, nil, consent.AudienceStudent, "2026.10.01")
		terms := notice(consent.KindTermsOfUse, nil, consent.AudienceAll, "2026.10.01")
		for _, n := range []consent.Notice{priv, terms} {
			if err := w.s.InsertNotice(ctx, n); err != nil {
				t.Fatal(err)
			}
			if err := w.s.Publish(ctx, n.ID, base); err != nil {
				t.Fatal(err)
			}
		}
		all, err := w.s.CurrentNotices(ctx, consent.CurrentQuery{})
		if err != nil {
			t.Fatal(err)
		}
		var ids []uuid.UUID
		for _, n := range all {
			ids = append(ids, n.ID)
		}
		if !slices.Equal(ids, []uuid.UUID{v2.ID, priv.ID, terms.ID}) {
			t.Fatalf("CurrentNotices = %v, want consent_text, privacy_notice, terms_of_use", ids)
		}
		kind := consent.KindPrivacyNotice
		some, _ := w.s.CurrentNotices(ctx, consent.CurrentQuery{Kind: &kind, Audiences: []consent.Audience{consent.AudienceAll}})
		if len(some) != 0 {
			t.Fatalf("filtered CurrentNotices = %v", some)
		}
		some, _ = w.s.CurrentNotices(ctx, consent.CurrentQuery{Purpose: &email, Locales: []string{consent.LocaleTR}})
		if len(some) != 1 || some[0].ID != v2.ID {
			t.Fatalf("CurrentNotices by purpose = %v", some)
		}
	})

	t.Run("subject lock (BE-062)", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		u := w.user(t)
		err := w.tx.WithTx(t.Context(), func(ctx context.Context) error {
			for range 2 { // re-entrant within the transaction
				if err := w.s.LockSubject(ctx, u, email); err != nil {
					return err
				}
			}
			if err := w.s.LockSubject(ctx, u, consent.PurposeCommercialPush); err != nil {
				return err
			}
			_, found, err := w.s.Latest(ctx, u, email, nil)
			if err == nil && found {
				err = errors.New("a fresh user has a consent")
			}
			return err
		})
		if err != nil {
			t.Fatalf("LockSubject: %v", err)
		}
	})

	t.Run("consent ledger", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		u1, u2, u3 := w.user(t), w.user(t), w.user(t)
		text := notice(consent.KindConsentText, &email, consent.AudienceAll, "2026.10.01")
		if err := w.s.InsertNotice(ctx, text); err != nil {
			t.Fatal(err)
		}
		at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
		row := func(u uuid.UUID, p consent.Purpose, a consent.Action, m int) consent.Consent {
			c := consent.Consent{ID: uuid.NewV7(), UserID: u, PurposeCode: p, Action: a, ScopeType: consent.ScopeNone,
				Channel: consent.ChannelWeb, OccurredAt: at(m)}
			if a == consent.Granted {
				c.NoticeVersionID, c.TextSHA256 = &text.ID, text.BodySHA256
			}
			return c
		}
		g1 := row(u1, email, consent.Granted, 1)
		g1.IP, g1.UserAgent = "2001:db8::7", "Koro/1.0"
		rows := []consent.Consent{
			g1, row(u1, email, consent.Withdrawn, 2), row(u1, email, consent.Granted, 3),
			row(u2, email, consent.Granted, 1), row(u2, email, consent.Withdrawn, 5),
			row(u3, email, consent.Withdrawn, 1), row(u1, consent.PurposeCommercialPush, consent.Withdrawn, 4),
		}
		for _, c := range rows {
			if err := w.s.InsertConsent(ctx, c); err != nil {
				t.Fatalf("InsertConsent: %v", err)
			}
		}
		reg := uuid.NewV7()
		scoped := row(u1, consent.PurposeEventSponsor, consent.Withdrawn, 6)
		scoped.ScopeType, scoped.ScopeID = consent.ScopeRegistration, &reg
		if err := w.s.InsertConsent(ctx, scoped); err != nil {
			t.Fatal(err)
		}

		got, err := w.s.ConsentByID(ctx, g1.ID)
		if err != nil || got.IP != "2001:db8::7" || got.UserAgent != "Koro/1.0" || !bytes.Equal(got.TextSHA256, text.BodySHA256) ||
			!got.OccurredAt.Equal(at(1)) || got.Channel != consent.ChannelWeb {
			t.Fatalf("ConsentByID = %+v, %v", got, err)
		}
		if _, err := w.s.ConsentByID(ctx, uuid.NewV7()); !errors.Is(err, consent.ErrNotFound) {
			t.Fatalf("missing consent = %v", err)
		}

		latest, found, err := w.s.Latest(ctx, u1, email, nil)
		if err != nil || !found || latest.ID != rows[2].ID {
			t.Fatalf("Latest(u1) = %v %v %v", latest.ID, found, err)
		}
		if _, found, _ := w.s.Latest(ctx, u3, consent.PurposeCommercialPush, nil); found {
			t.Fatal("Latest without rows found one")
		}
		if l, found, _ := w.s.Latest(ctx, u1, consent.PurposeEventSponsor, &reg); !found || l.ID != scoped.ID {
			t.Fatalf("Latest(scoped) = %v %v", l.ID, found)
		}
		if _, found, _ := w.s.Latest(ctx, u1, consent.PurposeEventSponsor, nil); found {
			t.Fatal("the unscoped Latest found a scoped row")
		}

		cur, err := w.s.CurrentOf(ctx, u1)
		if err != nil {
			t.Fatal(err)
		}
		var curIDs []uuid.UUID
		for _, c := range cur {
			curIDs = append(curIDs, c.ID)
		}
		if !slices.Equal(curIDs, []uuid.UUID{rows[2].ID, rows[6].ID, scoped.ID}) {
			t.Fatalf("CurrentOf(u1) = %v", curIDs)
		}

		granted, err := w.s.GrantedAmong(ctx, email, []uuid.UUID{u3, u2, u1, u1, uuid.NewV7()})
		if err != nil || !slices.Equal(granted, []uuid.UUID{u1}) {
			t.Fatalf("GrantedAmong = %v, %v", granted, err)
		}

		// Keyset pages, newest first.
		page1, err := w.s.ListConsents(ctx, consent.ListQuery{UserID: u1, Limit: 2})
		if err != nil || len(page1) != 2 || page1[0].ID != scoped.ID || page1[1].ID != rows[6].ID {
			t.Fatalf("page 1 = %v", page1)
		}
		last := page1[1]
		page2, _ := w.s.ListConsents(ctx, consent.ListQuery{UserID: u1, Limit: 10, After: &consent.After{At: last.OccurredAt, ID: last.ID}})
		if len(page2) != 3 || page2[0].ID != rows[2].ID || page2[2].ID != g1.ID {
			t.Fatalf("page 2 = %v", page2)
		}
		only, _ := w.s.ListConsents(ctx, consent.ListQuery{UserID: u1, Purpose: &email})
		if len(only) != 3 {
			t.Fatalf("filtered list = %d rows, want 3", len(only))
		}
		all, _ := w.s.AllOf(ctx, u1)
		if len(all) != 5 || all[0].ID != g1.ID || all[4].ID != scoped.ID {
			t.Fatalf("AllOf = %v", all)
		}

		// The CHECK constraints of 02 §4.3.
		noProof := row(u1, email, consent.Granted, 9)
		noProof.TextSHA256 = nil
		noScope := row(u1, consent.PurposeEventSponsor, consent.Withdrawn, 9)
		noScope.ScopeType = consent.ScopeRegistration
		strayScope := row(u1, email, consent.Withdrawn, 9)
		strayScope.ScopeID = &reg
		badChannel := row(u1, email, consent.Withdrawn, 9)
		badChannel.Channel = "pigeon"
		for i, c := range []consent.Consent{noProof, noScope, strayScope, badChannel} {
			if err := w.s.InsertConsent(ctx, c); !errors.Is(err, problem.ErrValidation) {
				t.Fatalf("bad consent %d = %v, want 422", i, err)
			}
		}
		unknown := row(u1, "selfie_share", consent.Withdrawn, 9)
		if err := w.s.InsertConsent(ctx, unknown); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("unknown purpose = %v, want 409 (FK)", err)
		}
	})

	t.Run("proof retention (BE-057)", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		text := notice(consent.KindConsentText, &email, consent.AudienceAll, "2020.01.01")
		if err := w.s.InsertNotice(ctx, text); err != nil {
			t.Fatal(err)
		}
		add := func(u uuid.UUID, a consent.Action, at time.Time) consent.Consent {
			t.Helper()
			c := consent.Consent{ID: uuid.NewV7(), UserID: u, PurposeCode: email, Action: a, ScopeType: consent.ScopeNone,
				Channel: consent.ChannelWeb, OccurredAt: at}
			if a == consent.Granted {
				c.NoticeVersionID, c.TextSHA256 = &text.ID, text.BodySHA256
			}
			if err := w.s.InsertConsent(ctx, c); err != nil {
				t.Fatal(err)
			}
			return c
		}
		long := base.AddDate(-6, 0, 0)
		gone, kept, live := w.user(t), w.user(t), w.user(t)
		g := add(gone, consent.Granted, long)
		add(gone, consent.Withdrawn, long.AddDate(0, 1, 0)) // withdrawn ~6 years ago: the chain goes
		sub := consent.Submission{ID: uuid.NewV7(), ConsentID: g.ID, Channel: consent.IYSEmail, Status: consent.SubmissionPending,
			CreatedAt: long, UpdatedAt: long}
		if err := w.s.InsertSubmission(ctx, sub); err != nil {
			t.Fatal(err)
		}
		add(kept, consent.Withdrawn, base.AddDate(-1, 0, 0)) // withdrawn a year ago: kept
		add(live, consent.Granted, long)                     // still granted: the proof in force
		cutoff := base.AddDate(-4, 0, 0)
		if n, err := w.s.PurgeProof(ctx, cutoff, 1000, true); err != nil || n != 2 {
			t.Fatalf("dry run = %d, %v; want the 2 rows of the withdrawn chain", n, err)
		}
		if rows, _ := w.s.AllOf(ctx, gone); len(rows) != 2 {
			t.Fatalf("a dry run deleted: %d rows left", len(rows))
		}
		if _, err := w.s.SubmissionOf(ctx, g.ID); err != nil {
			t.Fatalf("a dry run deleted the submission: %v", err)
		}
	})

	t.Run("iys submissions", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		ctx := t.Context()
		u := w.user(t)
		var subs []consent.Submission
		for range 3 {
			c := consent.Consent{ID: uuid.NewV7(), UserID: u, PurposeCode: email, Action: consent.Withdrawn,
				ScopeType: consent.ScopeNone, Channel: consent.ChannelEmailLink, OccurredAt: base}
			if err := w.s.InsertConsent(ctx, c); err != nil {
				t.Fatal(err)
			}
			n := consent.Submission{ID: uuid.NewV7(), ConsentID: c.ID, Channel: consent.IYSEmail, Status: consent.SubmissionPending,
				CreatedAt: base, UpdatedAt: base}
			if err := w.s.InsertSubmission(ctx, n); err != nil {
				t.Fatal(err)
			}
			subs = append(subs, n)
		}
		dup := subs[0]
		dup.ID = uuid.NewV7()
		if err := w.s.InsertSubmission(ctx, dup); !errors.Is(err, problem.ErrConflict) {
			t.Fatalf("second submission of a consent = %v, want 409", err)
		}
		got, err := w.s.SubmissionOf(ctx, subs[0].ConsentID)
		if err != nil || got.ID != subs[0].ID || got.Status != consent.SubmissionPending || got.Attempts != 0 {
			t.Fatalf("SubmissionOf = %+v, %v", got, err)
		}
		if _, err := w.s.SubmissionOf(ctx, uuid.NewV7()); !errors.Is(err, consent.ErrNotFound) {
			t.Fatalf("missing submission = %v", err)
		}

		sentAt, tx, msg := base.Add(time.Minute), "iys-tx-1", "timeout"
		err = w.tx.WithTx(ctx, func(ctx context.Context) error {
			s0, err := w.s.LockSubmission(ctx, subs[0].ID)
			if err != nil {
				return err
			}
			s0.Status, s0.SentAt, s0.IYSTransactionID, s0.Attempts = consent.SubmissionSent, &sentAt, &tx, 1
			if err := w.s.UpdateSubmission(ctx, s0); err != nil {
				return err
			}
			s1, _ := w.s.LockSubmission(ctx, subs[1].ID)
			s1.Status, s1.LastError, s1.Attempts = consent.SubmissionFailed, &msg, 1
			return w.s.UpdateSubmission(ctx, s1)
		})
		if err != nil {
			t.Fatal(err)
		}
		s0, _ := w.s.SubmissionOf(ctx, subs[0].ConsentID)
		if s0.Status != consent.SubmissionSent || !s0.SentAt.Equal(sentAt) || *s0.IYSTransactionID != tx {
			t.Fatalf("sent = %+v", s0)
		}
		bad := s0
		bad.SentAt = nil // iys_submissions_sent_chk
		if err := w.s.UpdateSubmission(ctx, bad); !errors.Is(err, problem.ErrValidation) {
			t.Fatalf("sent without sent_at = %v, want 422", err)
		}

		// Due = pending or failed, updated before the cutoff, in ID order.
		due, err := w.s.DueSubmissions(ctx, time.Now().Add(time.Hour), uuid.Nil(), 10)
		if err != nil || len(due) != 2 || due[0].ID != subs[1].ID || due[1].ID != subs[2].ID {
			t.Fatalf("DueSubmissions = %+v, %v", due, err)
		}
		next, _ := w.s.DueSubmissions(ctx, time.Now().Add(time.Hour), due[0].ID, 10)
		if len(next) != 1 || next[0].ID != subs[2].ID {
			t.Fatalf("DueSubmissions after = %+v", next)
		}
		if early, _ := w.s.DueSubmissions(ctx, base.Add(-time.Hour), uuid.Nil(), 10); len(early) != 0 {
			t.Fatalf("DueSubmissions before any update = %+v", early)
		}
	})
}

func TestStore_Contract_Mem(t *testing.T) {
	t.Parallel()
	storeContract(t, func(*testing.T) storeWorld {
		return storeWorld{
			s: consent.NewMemStore(time.Now), tx: db.NopTx{},
			user: func(*testing.T) uuid.UUID { return uuid.NewV7() },
		}
	})
}
