package feed

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
	"unicode/utf8"
	"uuid"

	"koro/internal/platform/problem"
)

// errCheck mirrors a CHECK violation of 02 §12 (the GORM store gets 422
// from db.Translate).
var errCheck = problem.ErrValidation.Wrap(errors.New("feed: check constraint violated"))

// MemStore is the in-memory Store of service, wiring and matrix tests
// (05 §13.2). It enforces the CHECKs of 02 §12.2–12.3 a test could
// otherwise meet only in PostgreSQL and the same visibility rules as the
// GORM queries. Every method is atomic under one mutex; it has no
// transactions (db.NopTx).
type MemStore struct {
	mu       sync.Mutex
	posts    map[uuid.UUID]Post
	media    map[uuid.UUID][]Media      // by post id
	dropped  map[uuid.UUID]DroppedMedia // post_dropped_media by file id (BE-158)
	settings map[uuid.UUID]OrgSettings
	follows  map[[2]uuid.UUID]Follow    // {user, org}
	mutes    map[[2]uuid.UUID]time.Time // {user, org} → created_at
	hides    map[[2]uuid.UUID]time.Time // {user, post} → created_at
	likes    map[[2]uuid.UUID]time.Time // {user, post} → created_at
	saves    map[[2]uuid.UUID]time.Time // {user, post} → created_at
	reports  map[uuid.UUID]Report       // content_reports (BE-032)
	actions  []ModerationAction         // moderation_actions, append-only (BE-032)
	// now stamps created_at/updated_at like the column defaults and the
	// touch trigger; updated_at always moves forward.
	now func() time.Time
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty in-memory Store; now stamps the rows.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{
		posts: map[uuid.UUID]Post{}, media: map[uuid.UUID][]Media{}, dropped: map[uuid.UUID]DroppedMedia{}, settings: map[uuid.UUID]OrgSettings{},
		follows: map[[2]uuid.UUID]Follow{}, mutes: map[[2]uuid.UUID]time.Time{}, hides: map[[2]uuid.UUID]time.Time{},
		likes: map[[2]uuid.UUID]time.Time{}, saves: map[[2]uuid.UUID]time.Time{}, reports: map[uuid.UUID]Report{},
		now: now,
	}
}

func (s *MemStore) stamp(prev time.Time) time.Time {
	t := s.now().UTC().Truncate(time.Microsecond)
	if !t.After(prev) {
		t = prev.Add(time.Microsecond)
	}
	return t
}

func runes(p *string) int {
	if p == nil {
		return 0
	}
	return utf8.RuneCountInString(*p)
}

// checkPost applies the CHECK constraints of posts (02 §12.2).
func checkPost(p Post) error {
	switch {
	case !p.AuthorOrgKind.Valid(), !p.Kind.Valid(), !p.AudienceKind.Valid(), !p.Status.Valid(),
		runes(p.Title) > 140, runes(p.BodyText) > 5000, p.AudienceDimCount < 1, p.AudienceDimCount > 4,
		p.LikeCount < 0, p.SaveCount < 0:
		return errCheck
	case (p.Kind == KindEventShare) != (p.EventID != nil),
		(p.Kind == KindOpportunityCall) != (p.Opportunity != nil),
		p.Kind != KindImage && p.Kind != KindEventShare && p.BodyJSON == nil,
		p.AuthorOrgKind == AuthorCompany && !p.CommercialFlag:
		return errCheck
	case p.AudienceKind != AudienceSegment && (!slices.Equal(p.AudienceKeys, []string{KeyAll}) || p.AudienceDimCount != 1),
		p.AudienceKind == AudienceSegment && p.AudienceSnapshot == nil,
		p.AudienceKind == AudienceSegment && p.AuthorOrgKind == AuthorCompany &&
			(p.AudienceSizeEstimate == nil || *p.AudienceSizeEstimate < 10 || p.EntitlementUsageID == nil):
		return errCheck
	case (p.Status == StatusPublished || p.Status == StatusHidden) && p.PublishedAt == nil,
		(p.Status == StatusRemoved) != (p.RemovedAt != nil):
		return errCheck
	}
	return nil
}

// PutPost stores p as it is (fixtures of the matrix and of tests): no
// stamping, the CHECKs apply.
func (s *MemStore) PutPost(p Post) error {
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	if err := checkPost(p); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts[p.ID] = p.clone()
	return nil
}

// PutSettings stores the feed settings of an org (fixtures).
func (s *MemStore) PutSettings(o OrgSettings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[o.OrgID] = o
}

// PutFollow / PutMute / PutHide / PutLike / PutSave seed the reader rows
// (fixtures and tests) without touching the counters: a seeded like or
// save is a drift feed.reconcile_counters corrects.
func (s *MemStore) PutFollow(userID, orgID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.follows[[2]uuid.UUID{userID, orgID}] = Follow{UserID: userID, OrgID: orgID, Source: FollowManual,
		CreatedAt: s.stamp(time.Time{})}
}

// PutMute seeds an org_mutes row.
func (s *MemStore) PutMute(userID, orgID uuid.UUID) { s.put(s.mutes, userID, orgID) }

// PutHide seeds a post_hides row.
func (s *MemStore) PutHide(userID, postID uuid.UUID) { s.put(s.hides, userID, postID) }

// PutLike seeds a post_reactions row.
func (s *MemStore) PutLike(userID, postID uuid.UUID) { s.put(s.likes, userID, postID) }

// PutSave seeds a post_saves row.
func (s *MemStore) PutSave(userID, postID uuid.UUID) { s.put(s.saves, userID, postID) }

func (s *MemStore) put(m map[[2]uuid.UUID]time.Time, a, b uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m[[2]uuid.UUID{a, b}] = s.stamp(time.Time{})
}

// InsertPost implements Store.
func (s *MemStore) InsertPost(_ context.Context, p Post) (Post, error) {
	if p.ID == uuid.Nil() || p.OrgID == uuid.Nil() || p.AuthorUserID == uuid.Nil() {
		return Post{}, errors.New("feed: insert post: missing id")
	}
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	if err := checkPost(p); err != nil {
		return Post{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.posts[p.ID]; dup {
		return Post{}, problem.ErrConflict
	}
	p.CreatedAt = s.stamp(time.Time{})
	p.UpdatedAt, p.LikeCount, p.SaveCount = p.CreatedAt, 0, 0
	s.posts[p.ID] = p.clone()
	return p.clone(), nil
}

// GetPost implements Store.
func (s *MemStore) GetPost(_ context.Context, id uuid.UUID) (Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.posts[id]
	if !ok {
		return Post{}, ErrNotFound
	}
	return p.clone(), nil
}

// LockPost implements Store.
func (s *MemStore) LockPost(ctx context.Context, id uuid.UUID) (Post, error) {
	return s.GetPost(ctx, id)
}

// UpdatePost implements Store.
func (s *MemStore) UpdatePost(_ context.Context, p Post) (Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.posts[p.ID]
	if !ok || cur.OrgID != p.OrgID {
		return Post{}, ErrNotFound
	}
	next := p.clone()
	next.AuthorOrgKind, next.AuthorUserID, next.CreatedAt = cur.AuthorOrgKind, cur.AuthorUserID, cur.CreatedAt
	next.LikeCount, next.SaveCount = cur.LikeCount, cur.SaveCount
	if next.SchemaVersion == 0 {
		next.SchemaVersion = 1
	}
	if err := checkPost(next); err != nil {
		return Post{}, err
	}
	next.UpdatedAt = s.stamp(cur.UpdatedAt)
	s.posts[p.ID] = next
	return next.clone(), nil
}

// DeletePost implements Store.
func (s *MemStore) DeletePost(_ context.Context, orgID, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.posts[id]
	if !ok || p.OrgID != orgID || (p.Status != StatusDraft && p.Status != StatusPendingReview) {
		return ErrNotFound
	}
	delete(s.posts, id)
	delete(s.media, id)
	s.dropPostRows(id)
	for _, m := range []map[[2]uuid.UUID]time.Time{s.hides, s.likes, s.saves} {
		for k := range m {
			if k[1] == id {
				delete(m, k)
			}
		}
	}
	return nil
}

// ReplaceMedia implements Store.
func (s *MemStore) ReplaceMedia(_ context.Context, orgID, id uuid.UUID, media []Media) error {
	if len(media) > 10 {
		return errCheck
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.posts[id]
	if !ok || p.OrgID != orgID {
		return ErrNotFound
	}
	out := make([]Media, len(media))
	for i, m := range media {
		if runes(m.AltText) > 300 || m.Width != nil && *m.Width <= 0 || m.Height != nil && *m.Height <= 0 {
			return errCheck
		}
		for pid, ms := range s.media { // post_media_file_id_uq
			if pid != id && slices.ContainsFunc(ms, func(x Media) bool { return x.FileID == m.FileID }) {
				return problem.ErrConflict
			}
		}
		m.PostID, m.Position, m.CreatedAt = id, int16(i), s.stamp(time.Time{})
		out[i] = m
	}
	s.media[id] = out
	return nil
}

// MediaOf implements Store.
func (s *MemStore) MediaOf(_ context.Context, postIDs []uuid.UUID) (map[uuid.UUID][]Media, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID][]Media{}
	for _, id := range postIDs {
		if ms := s.media[id]; len(ms) > 0 {
			out[id] = slices.Clone(ms)
		}
	}
	return out, nil
}

// DropMedia implements Store (the CHECKs and the file_id key of
// post_dropped_media).
func (s *MemStore) DropMedia(_ context.Context, orgID, id uuid.UUID, dropped []DroppedMedia) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.posts[id]
	if !ok || p.OrgID != orgID {
		return ErrNotFound
	}
	for _, m := range dropped {
		if runes(m.AltText) > 300 || m.Width != nil && *m.Width <= 0 || m.Height != nil && *m.Height <= 0 {
			return errCheck
		}
		if _, taken := s.dropped[m.FileID]; taken {
			return problem.ErrConflict
		}
	}
	for _, m := range dropped {
		m.PostID, m.DroppedAt = id, m.DroppedAt.UTC().Truncate(time.Microsecond)
		s.dropped[m.FileID] = m
	}
	return nil
}

// DroppedMediaOf implements Store.
func (s *MemStore) DroppedMediaOf(_ context.Context, postIDs []uuid.UUID) (map[uuid.UUID][]DroppedMedia, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID][]DroppedMedia{}
	for _, m := range s.dropped {
		if slices.Contains(postIDs, m.PostID) {
			out[m.PostID] = append(out[m.PostID], m)
		}
	}
	for _, ms := range out {
		slices.SortFunc(ms, compareDropped)
	}
	return out, nil
}

// ForgetDroppedMedia implements Store.
func (s *MemStore) ForgetDroppedMedia(_ context.Context, fileIDs []uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range fileIDs {
		if _, ok := s.dropped[id]; ok {
			delete(s.dropped, id)
			n++
		}
	}
	return n, nil
}

// dropPostRows is the ON DELETE CASCADE of post_dropped_media; the caller
// holds the lock.
func (s *MemStore) dropPostRows(postID uuid.UUID) {
	for id, m := range s.dropped {
		if m.PostID == postID {
			delete(s.dropped, id)
		}
	}
}

// compareDropped orders dropped images oldest first.
func compareDropped(a, b DroppedMedia) int {
	return cmp.Or(a.DroppedAt.Compare(b.DroppedAt), a.FileID.Compare(b.FileID))
}

// visible is readerConds of the GORM store.
func (s *MemStore) visible(r Reader, p Post) bool {
	switch {
	case r.All:
		return true
	case r.UserID == nil:
		return p.AudienceKind == AudiencePublic && !p.CommercialFlag
	case !audienceMatch(p.AudienceKeys, p.AudienceDimCount, r.Keys), r.Minor && p.CommercialFlag:
		return false
	case p.AudienceKind == AudienceFollowers:
		return slices.Contains(r.MemberOrgs, p.OrgID) || has(s.follows, *r.UserID, p.OrgID)
	}
	return true
}

// ListFeed implements Store.
func (s *MemStore) ListFeed(_ context.Context, f FeedFilter, after *After, limit int) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Following && f.UserID == nil {
		return []Post{}, nil
	}
	var out []Post
	for _, p := range s.posts {
		if p.Status != StatusPublished || !s.visible(f.Reader, p) || f.Kind != "" && p.Kind != f.Kind {
			continue
		}
		if u := f.UserID; u != nil {
			followed := slices.Contains(f.MemberOrgs, p.OrgID) || has(s.follows, *u, p.OrgID)
			if has(s.mutes, *u, p.OrgID) || has(s.hides, *u, p.ID) || f.Following && !followed {
				continue
			}
		}
		out = append(out, p.clone())
	}
	return pagePublished(out, after, limit), nil
}

// ListOrgPosts implements Store.
func (s *MemStore) ListOrgPosts(_ context.Context, orgID uuid.UUID, r Reader, after *After, limit int) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Post
	for _, p := range s.posts {
		if p.OrgID == orgID && p.Status == StatusPublished && s.visible(r, p) {
			out = append(out, p.clone())
		}
	}
	return pagePublished(out, after, limit), nil
}

// ListEventShares implements Store.
func (s *MemStore) ListEventShares(_ context.Context, eventIDs []uuid.UUID, r Reader, limit int) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Post
	for _, p := range s.posts {
		if p.EventID != nil && slices.Contains(eventIDs, *p.EventID) && p.Status == StatusPublished && s.visible(r, p) {
			out = append(out, p.clone())
		}
	}
	return pagePublished(out, nil, limit), nil
}

// pagePublished sorts newest first by (published_at, id) and cuts the
// page after the keyset position.
func pagePublished(ps []Post, after *After, limit int) []Post {
	slices.SortFunc(ps, func(a, b Post) int {
		return -cmp.Or(a.PublishedAt.Compare(*b.PublishedAt), a.ID.Compare(b.ID))
	})
	if after != nil {
		ps = slices.DeleteFunc(ps, func(p Post) bool {
			return cmp.Or(p.PublishedAt.Compare(after.At), p.ID.Compare(after.ID)) >= 0
		})
	}
	if len(ps) > limit {
		ps = ps[:limit]
	}
	if ps == nil {
		ps = []Post{}
	}
	return ps
}

// ListOrgManage implements Store.
func (s *MemStore) ListOrgManage(_ context.Context, orgID uuid.UUID, f ManageFilter, after *uuid.UUID, limit int) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Post{}
	for _, p := range s.posts {
		if p.OrgID != orgID || len(f.Statuses) > 0 && !slices.Contains(f.Statuses, p.Status) || f.Kind != "" && p.Kind != f.Kind {
			continue
		}
		if after != nil && p.ID.Compare(*after) >= 0 {
			continue
		}
		out = append(out, p.clone())
	}
	slices.SortFunc(out, func(a, b Post) int { return -a.ID.Compare(b.ID) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// OrgSettings implements Store.
func (s *MemStore) OrgSettings(_ context.Context, orgID uuid.UUID) (OrgSettings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.settings[orgID]
	return o, ok, nil
}

// Follows implements Store.
func (s *MemStore) Follows(_ context.Context, userID, orgID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return has(s.follows, userID, orgID), nil
}

// ViewerState implements Store.
func (s *MemStore) ViewerState(_ context.Context, userID uuid.UUID, postIDs, orgIDs []uuid.UUID) (ViewerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := newViewerState()
	for _, id := range postIDs {
		if has(s.likes, userID, id) {
			out.Liked[id] = true
		}
		if has(s.saves, userID, id) {
			out.Saved[id] = true
		}
	}
	for _, id := range orgIDs {
		if has(s.follows, userID, id) {
			out.Following[id] = true
		}
		if has(s.mutes, userID, id) {
			out.Muted[id] = true
		}
	}
	return out, nil
}

// PostsOfAuthor implements Store.
func (s *MemStore) PostsOfAuthor(_ context.Context, userID uuid.UUID) ([]Post, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Post{}
	for _, p := range s.posts {
		if p.AuthorUserID == userID {
			out = append(out, p.clone())
		}
	}
	slices.SortFunc(out, func(a, b Post) int { return a.ID.Compare(b.ID) })
	return out, nil
}
