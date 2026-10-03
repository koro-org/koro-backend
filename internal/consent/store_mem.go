package consent

import (
	"bytes"
	"cmp"
	"context"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
)

// memStore is the in-memory Store of service, wiring and matrix tests. It
// implements the GORM store's contract (store_contract_test.go), including
// the seed of migration 000011, the CHECK and unique constraints a test
// could otherwise only meet in PostgreSQL, the published-notice guard and
// the append-only ledger. Locks do not lock: tests run one operation at a
// time per row.
type memStore struct {
	mu       sync.Mutex
	purposes map[Purpose]PurposeDef
	notices  map[uuid.UUID]Notice
	consents []Consent // insertion order
	subs     map[uuid.UUID]Submission
	// now stamps created_at/updated_at like the column defaults and the
	// iys_submissions_touch_updated_at trigger.
	now func() time.Time
}

// NewMemStore returns an in-memory Store holding the purposes of migration
// 000011; now stamps the timestamps the database would set.
func NewMemStore(now func() time.Time) Store {
	s := &memStore{
		purposes: map[Purpose]PurposeDef{}, notices: map[uuid.UUID]Notice{},
		subs: map[uuid.UUID]Submission{}, now: now,
	}
	for _, p := range seedPurposes() {
		p.CreatedAt = now()
		s.purposes[p.Code] = p
	}
	return s
}

// seedPurposes mirrors migration 000011 (02 §4.1); the store contract test
// pins both to the same rows.
func seedPurposes() []PurposeDef {
	email, sms := IYSEmail, IYSSMS
	return []PurposeDef{
		{Code: PurposeCommercialEmail, LegalBasis: BasisExplicitConsent, RequiresIYS: true, IYSChannel: &email, ScopeType: ScopeNone, Active: true,
			DescriptionTR: "Kampanya, duyuru ve fırsat içeren ticari elektronik iletileri e-posta ile alma izni (İYS kaydı yapılır)."},
		{Code: PurposeCommercialSMS, LegalBasis: BasisExplicitConsent, RequiresIYS: true, IYSChannel: &sms, ScopeType: ScopeNone, Active: true,
			DescriptionTR: "Kampanya, duyuru ve fırsat içeren ticari elektronik iletileri SMS ile alma izni (v1 sürümünde SMS kanalı yoktur; kayıt hazırdır)."},
		{Code: PurposeCommercialPush, LegalBasis: BasisExplicitConsent, ScopeType: ScopeNone, Active: true,
			DescriptionTR: "Kampanya, duyuru ve fırsat içeren ticari bildirimleri mobil bildirim olarak alma izni."},
		{Code: PurposeDiscoveryProfile, LegalBasis: BasisExplicitConsent, ScopeType: ScopeNone, Active: true,
			DescriptionTR: "Profilimin takma adla, şirketlerin keşif ekranında görünmesine izin veriyorum (Seviye 2)."},
		{Code: PurposeContactInfoShare, LegalBasis: BasisExplicitConsent, ScopeType: ScopeNone, Active: true,
			DescriptionTR: "Bir iletişim talebini kabul ettiğimde iletişim bilgilerimin talep sahibiyle paylaşılmasına izin veriyorum."},
		{Code: PurposeEventSponsor, LegalBasis: BasisExplicitConsent, ScopeType: ScopeRegistration, Active: true,
			DescriptionTR: "Katıldığım etkinliğin sponsoruyla katılım bilgilerimin paylaşılmasına izin veriyorum (kayıt başına)."},
		{Code: PurposeActivityRanking, LegalBasis: BasisExplicitConsent, ScopeType: ScopeNone, Active: false,
			DescriptionTR: "Etkinlik ve topluluk aktivitelerime dayalı sıralama (talent) için verilerimin işlenmesine izin veriyorum."},
	}
}

// Constraint violations of the in-memory store, as the GORM store reports
// them for PostgreSQL (CHECK → 422, unique and foreign keys → 409).
var (
	errCheck  = problem.ErrValidation
	errUnique = problem.ErrConflict
)

// versionRe is notice_versions_version_chk.
var versionRe = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]{2}(-[0-9]+)?$`)

func (s *memStore) Purposes(context.Context) ([]PurposeDef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.SortedFunc(maps.Values(s.purposes), func(a, b PurposeDef) int { return cmp.Compare(a.Code, b.Code) }), nil
}

func (s *memStore) Purpose(_ context.Context, code Purpose) (PurposeDef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.purposes[code]
	if !ok {
		return PurposeDef{}, ErrUnknownPurpose
	}
	return p, nil
}

func (s *memStore) LockPurpose(ctx context.Context, code Purpose) (PurposeDef, error) {
	return s.Purpose(ctx, code)
}

func (s *memStore) UpdatePurpose(_ context.Context, p PurposeDef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.purposes[p.Code]
	if !ok {
		return ErrUnknownPurpose
	}
	cur.DescriptionTR, cur.Active = p.DescriptionTR, p.Active
	s.purposes[p.Code] = cur
	return nil
}

func (s *memStore) checkNotice(n Notice) error {
	switch n.Kind {
	case KindPrivacyNotice, KindConsentText, KindTermsOfUse, KindCookieNotice:
	default:
		return errCheck
	}
	switch n.Audience {
	case AudienceAll, AudienceStudent, AudienceCompany, AudienceClub, AudienceUniversity:
	default:
		return errCheck
	}
	switch {
	case n.Locale != LocaleTR && n.Locale != LocaleEN,
		!versionRe.MatchString(n.Version),
		utf8.RuneCountInString(n.Title) > 200,
		len(n.BodySHA256) != 32,
		(n.Kind == KindConsentText) != (n.PurposeCode != nil): // notice_versions_purpose_chk
		return errCheck
	}
	if n.PurposeCode != nil {
		if _, ok := s.purposes[*n.PurposeCode]; !ok {
			return ErrUnknownPurpose // notice_versions_purpose_code_fkey
		}
	}
	return nil
}

func (s *memStore) InsertNotice(_ context.Context, n Notice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.notices[n.ID]; dup {
		return errUnique
	}
	if err := s.checkNotice(n); err != nil {
		return err
	}
	for _, o := range s.notices {
		if o.Key().same(n.Key()) && o.Version == n.Version {
			return ErrNoticeVersionTaken // notice_versions_version_uq
		}
	}
	n = n.clone()
	n.PublishedAt, n.RetiredAt = nil, nil
	if n.CreatedAt.IsZero() {
		n.CreatedAt = s.now()
	}
	s.notices[n.ID] = n
	return nil
}

func (s *memStore) Notice(_ context.Context, id uuid.UUID) (Notice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notices[id]
	if !ok {
		return Notice{}, ErrNotFound
	}
	return n.clone(), nil
}

func (s *memStore) LockNotice(ctx context.Context, id uuid.UUID) (Notice, error) {
	return s.Notice(ctx, id)
}

func (s *memStore) LockCurrent(_ context.Context, key NoticeKey) (Notice, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notices {
		if n.Status() == StatusPublished && n.Key().same(key) {
			return n.clone(), true, nil
		}
	}
	return Notice{}, false, nil
}

func (s *memStore) CurrentNotices(_ context.Context, q CurrentQuery) ([]Notice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Notice
	for _, n := range s.notices {
		switch {
		case n.Status() != StatusPublished,
			q.Kind != nil && n.Kind != *q.Kind,
			q.Purpose != nil && (n.PurposeCode == nil || *n.PurposeCode != *q.Purpose),
			len(q.Audiences) > 0 && !slices.Contains(q.Audiences, n.Audience),
			len(q.Locales) > 0 && !slices.Contains(q.Locales, n.Locale):
			continue
		}
		out = append(out, n.clone())
	}
	slices.SortFunc(out, compareNoticeKeys)
	return out, nil
}

// compareNoticeKeys orders notices by kind, purpose, audience and locale
// (the ORDER BY of the GORM store).
func compareNoticeKeys(a, b Notice) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key().purpose(), b.Key().purpose()),
		cmp.Compare(a.Audience, b.Audience), cmp.Compare(a.Locale, b.Locale))
}

func (s *memStore) Publish(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notices[id]
	if !ok || n.PublishedAt != nil {
		return ErrNoticeState
	}
	for _, o := range s.notices {
		if o.ID != id && o.Status() == StatusPublished && o.Key().same(n.Key()) {
			return ErrNoticeState // notice_versions_current_uq
		}
	}
	n.PublishedAt = &at
	s.notices[id] = n
	return nil
}

func (s *memStore) Retire(_ context.Context, id uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.notices[id]
	if !ok || n.Status() != StatusPublished {
		return ErrNoticeState
	}
	n.RetiredAt = &at
	s.notices[id] = n
	return nil
}

func (s *memStore) checkConsent(c Consent) error {
	_, ok := s.purposes[c.PurposeCode]
	switch {
	case !ok:
		return errUnique // consents_purpose_code_fkey
	case c.Action != Granted && c.Action != Withdrawn,
		c.TextSHA256 != nil && len(c.TextSHA256) != 32,
		c.Action == Granted && (c.NoticeVersionID == nil || c.TextSHA256 == nil), // consents_proof_chk
		c.ScopeType != ScopeNone && c.ScopeType != ScopeRegistration,
		(c.ScopeType == ScopeNone) != (c.ScopeID == nil), // consents_scope_chk
		utf8.RuneCountInString(c.UserAgent) > 400:
		return errCheck
	}
	switch c.Channel {
	case ChannelWeb, ChannelMobile, ChannelEmailLink, ChannelAdmin, ChannelImport:
	default:
		return errCheck
	}
	if c.NoticeVersionID != nil {
		if _, ok := s.notices[*c.NoticeVersionID]; !ok {
			return errUnique // consents_notice_version_id_fkey
		}
	}
	return nil
}

func (s *memStore) InsertConsent(_ context.Context, c Consent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.consents, func(o Consent) bool { return o.ID == c.ID }) {
		return errUnique
	}
	if err := s.checkConsent(c); err != nil {
		return err
	}
	c = c.clone()
	c.CreatedAt = s.now()
	s.consents = append(s.consents, c)
	return nil
}

// newestFirst orders consents by occurred_at DESC, id DESC.
func newestFirst(a, b Consent) int {
	if c := b.OccurredAt.Compare(a.OccurredAt); c != 0 {
		return c
	}
	return bytes.Compare(b.ID[:], a.ID[:])
}

func sameScope(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (s *memStore) Latest(_ context.Context, userID uuid.UUID, purpose Purpose, scopeID *uuid.UUID) (Consent, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var best *Consent
	for i := range s.consents {
		c := &s.consents[i]
		if c.UserID != userID || c.PurposeCode != purpose || !sameScope(c.ScopeID, scopeID) {
			continue
		}
		if best == nil || newestFirst(*c, *best) < 0 {
			best = c
		}
	}
	if best == nil {
		return Consent{}, false, nil
	}
	return best.clone(), true, nil
}

func (s *memStore) CurrentOf(_ context.Context, userID uuid.UUID) ([]Consent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type key struct {
		p     Purpose
		scope uuid.UUID
	}
	latest := map[key]Consent{}
	for _, c := range s.consents {
		if c.UserID != userID {
			continue
		}
		k := key{p: c.PurposeCode, scope: uuid.Nil()}
		if c.ScopeID != nil {
			k.scope = *c.ScopeID
		}
		if cur, ok := latest[k]; !ok || newestFirst(c, cur) < 0 {
			latest[k] = c
		}
	}
	out := make([]Consent, 0, len(latest))
	for _, c := range latest {
		out = append(out, c.clone())
	}
	slices.SortFunc(out, compareCurrent)
	return out, nil
}

// compareCurrent orders current rows by purpose, then scope (unscoped first).
func compareCurrent(a, b Consent) int {
	if c := cmp.Compare(a.PurposeCode, b.PurposeCode); c != 0 {
		return c
	}
	switch {
	case a.ScopeID == nil && b.ScopeID == nil:
		return 0
	case a.ScopeID == nil:
		return -1
	case b.ScopeID == nil:
		return 1
	}
	return bytes.Compare(a.ScopeID[:], b.ScopeID[:])
}

func (s *memStore) GrantedAmong(ctx context.Context, purpose Purpose, userIDs []uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, id := range sortedIDs(userIDs) {
		c, ok, err := s.Latest(ctx, id, purpose, nil)
		if err != nil {
			return nil, err
		}
		if ok && c.Action == Granted {
			out = append(out, id)
		}
	}
	return out, nil
}

// sortedIDs returns the distinct IDs in ascending order.
func sortedIDs(ids []uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	return slices.Compact(out)
}

func (s *memStore) ListConsents(_ context.Context, q ListQuery) ([]Consent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []Consent
	for _, c := range s.consents {
		if c.UserID == q.UserID && (q.Purpose == nil || c.PurposeCode == *q.Purpose) {
			rows = append(rows, c)
		}
	}
	slices.SortFunc(rows, newestFirst)
	var out []Consent
	for _, c := range rows {
		if q.After != nil && newestFirst(c, Consent{OccurredAt: q.After.At, ID: q.After.ID}) <= 0 {
			continue
		}
		out = append(out, c.clone())
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out, nil
}

func (s *memStore) ConsentByID(_ context.Context, id uuid.UUID) (Consent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.consents {
		if c.ID == id {
			return c.clone(), nil
		}
	}
	return Consent{}, ErrNotFound
}

func (s *memStore) AllOf(_ context.Context, userID uuid.UUID) ([]Consent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Consent
	for _, c := range s.consents {
		if c.UserID == userID {
			out = append(out, c.clone())
		}
	}
	slices.SortFunc(out, func(a, b Consent) int { return newestFirst(b, a) })
	return out, nil
}

func (s *memStore) checkSubmission(n Submission) error {
	switch {
	case n.Channel != IYSEmail && n.Channel != IYSSMS && n.Channel != IYSCall,
		n.Attempts < 0,
		n.LastError != nil && utf8.RuneCountInString(*n.LastError) > 2000,
		n.Status == SubmissionSent && n.SentAt == nil: // iys_submissions_sent_chk
		return errCheck
	}
	switch n.Status {
	case SubmissionPending, SubmissionSent, SubmissionFailed, SubmissionSkipped:
	default:
		return errCheck
	}
	return nil
}

func (s *memStore) InsertSubmission(_ context.Context, n Submission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkSubmission(n); err != nil {
		return err
	}
	if !slices.ContainsFunc(s.consents, func(c Consent) bool { return c.ID == n.ConsentID }) {
		return errUnique // iys_submissions_consent_id_fkey
	}
	for _, o := range s.subs {
		if o.ID == n.ID || o.ConsentID == n.ConsentID {
			return errUnique // iys_submissions_consent_id_uq
		}
	}
	now := s.now()
	n.CreatedAt, n.UpdatedAt = now, now
	s.subs[n.ID] = n
	return nil
}

func (s *memStore) SubmissionOf(_ context.Context, consentID uuid.UUID) (Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.subs {
		if n.ConsentID == consentID {
			return n, nil
		}
	}
	return Submission{}, ErrNotFound
}

func (s *memStore) LockSubmission(_ context.Context, id uuid.UUID) (Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.subs[id]
	if !ok {
		return Submission{}, ErrNotFound
	}
	return n, nil
}

func (s *memStore) UpdateSubmission(_ context.Context, n Submission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.subs[n.ID]
	if !ok {
		return ErrNotFound
	}
	if err := s.checkSubmission(n); err != nil {
		return err
	}
	cur.Status, cur.IYSTransactionID, cur.Attempts, cur.LastError, cur.SentAt = n.Status, n.IYSTransactionID, n.Attempts, n.LastError, n.SentAt
	cur.UpdatedAt = s.now()
	s.subs[n.ID] = cur
	return nil
}

func (s *memStore) DueSubmissions(_ context.Context, before time.Time, after uuid.UUID, limit int) ([]Submission, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Submission
	for _, id := range slices.SortedFunc(maps.Keys(s.subs), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) {
		n := s.subs[id]
		if bytes.Compare(id[:], after[:]) > 0 && n.Status.open() && n.UpdatedAt.Before(before) {
			out = append(out, n)
			if limit > 0 && len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
