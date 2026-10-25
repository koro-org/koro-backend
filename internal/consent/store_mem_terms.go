package consent

import (
	"bytes"
	"cmp"
	"context"
	"regexp"
	"slices"
	"time"
	"uuid"
)

// requestIDRe is terms_acceptances_request_id_chk.
var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9_:-]{1,80}$`)

// checkTerms applies the CHECK and FK constraints of terms_acceptances
// (000148) the in-memory store can see: the notice of the composite FK is a
// terms of use with this body digest; the users FK is the database's.
func (s *memStore) checkTerms(t TermsAcceptance) error {
	switch {
	case len(t.TextSHA256) != 32, // terms_acceptances_text_sha256_chk
		t.Channel != ChannelWeb && t.Channel != ChannelMobile, // terms_acceptances_channel_chk
		t.RequestID != "" && !requestIDRe.MatchString(t.RequestID),
		t.AcceptedAt.IsZero():
		return errCheck
	}
	n, ok := s.notices[t.NoticeVersionID]
	if !ok || n.Kind != KindTermsOfUse || !bytes.Equal(n.BodySHA256, t.TextSHA256) {
		return errUnique // terms_acceptances_notice_fkey
	}
	return nil
}

// InsertTermsAcceptance implements TermsStore.
func (s *memStore) InsertTermsAcceptance(_ context.Context, t TermsAcceptance) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.terms, func(o TermsAcceptance) bool { return o.ID == t.ID }) {
		return false, errUnique
	}
	if err := s.checkTerms(t); err != nil {
		return false, err
	}
	if slices.ContainsFunc(s.terms, func(o TermsAcceptance) bool {
		return o.UserID == t.UserID && o.NoticeVersionID == t.NoticeVersionID
	}) {
		return false, nil // terms_acceptances_user_notice_uq: ON CONFLICT DO NOTHING
	}
	t = t.clone()
	t.CreatedAt = s.now()
	s.terms = append(s.terms, t)
	return true, nil
}

// TermsAcceptancesOf implements TermsStore.
func (s *memStore) TermsAcceptancesOf(_ context.Context, userID uuid.UUID) ([]TermsAcceptance, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []TermsAcceptance
	for _, t := range s.terms {
		if t.UserID == userID {
			out = append(out, t.clone())
		}
	}
	slices.SortFunc(out, func(a, b TermsAcceptance) int {
		return cmp.Or(a.AcceptedAt.Compare(b.AcceptedAt), a.ID.Compare(b.ID))
	})
	return out, nil
}

// PurgeTerms implements TermsStore. The in-memory store knows no accounts,
// so no acceptance belongs to an erased one: nothing is purged (the rule is
// the GORM store's, through users).
func (*memStore) PurgeTerms(context.Context, time.Time, int, bool) (int, error) { return 0, nil }
