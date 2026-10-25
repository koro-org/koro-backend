package consent_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"uuid"

	"koro/internal/consent"
)

// termsWatch records the terms of use versions consent announces.
type termsWatch struct {
	mu  sync.Mutex
	ids []uuid.UUID
}

func (w *termsWatch) TermsPublished(_ context.Context, id uuid.UUID) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ids = append(w.ids, id)
}

func (w *termsWatch) seen() []uuid.UUID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ids)
}

// TestPublishNotice_AnnouncesTermsToWatcher (BE-124): publishing a terms of
// use version tells Deps.TermsWatch, once, with the published version;
// publishing another kind of notice, or failing to publish, tells nobody.
// Without a watcher the publication is unchanged.
func TestPublishNotice_AnnouncesTermsToWatcher(t *testing.T) {
	t.Parallel()
	w := &termsWatch{}
	e := newEnv(t, func(d *consent.Deps) { d.TermsWatch = w })
	ctx := t.Context()

	v1 := e.terms(t, consent.AudienceAll, "2026.10.01")
	e.publish(t, consent.NoticeDraft{Kind: consent.KindPrivacyNotice, Audience: consent.AudienceStudent, Locale: consent.LocaleTR,
		Version: "2026.10.01", Title: "Aydınlatma metni", BodyMD: "Aydınlatma metni\n"})
	e.consentText(t, consent.PurposeCommercialEmail, "2026.10.01")
	v2 := e.terms(t, consent.AudienceStudent, "2026.10.02")
	if got := w.seen(); !slices.Equal(got, []uuid.UUID{v1.ID, v2.ID}) {
		t.Fatalf("announced %v, want the two terms versions %v", got, []uuid.UUID{v1.ID, v2.ID})
	}

	// Publishing a published version again fails and announces nothing.
	if _, err := e.svc.PublishNotice(ctx, actor(t, "dpo"), v2.ID, proof); !errors.Is(err, consent.ErrNoticeState) {
		t.Fatalf("second publication = %v, want consent.notice_state", err)
	}
	if got := w.seen(); len(got) != 2 {
		t.Fatalf("a failed publication was announced: %v", got)
	}

	// No watcher: the terms are published all the same.
	plain := newEnv(t)
	if n := plain.terms(t, consent.AudienceAll, "2026.10.01"); n.Status() != consent.StatusPublished {
		t.Fatalf("published without a watcher = %s", n.Status())
	}
}
