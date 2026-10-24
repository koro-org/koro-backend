package feed

import (
	"cmp"
	"context"
	"slices"
	"time"
	"uuid"
)

// has reports whether the pair {a, b} has a row in m.
func has[V any](m map[[2]uuid.UUID]V, a, b uuid.UUID) bool {
	_, ok := m[[2]uuid.UUID{a, b}]
	return ok
}

// counter moves a post counter like the GORM statements: only when the row
// changed, never below zero; updated_at stays (the counters are not part of
// the ETag).
func (s *MemStore) counter(postID uuid.UUID, like bool, delta int) {
	p, ok := s.posts[postID]
	if !ok {
		return
	}
	c := &p.SaveCount
	if like {
		c = &p.LikeCount
	}
	if *c+delta >= 0 {
		*c += delta
	}
	s.posts[postID] = p
}

// add inserts {user, post} into m when the post exists (the FK of the
// reader tables) and reports whether the row is new.
func (s *MemStore) add(m map[[2]uuid.UUID]time.Time, userID, postID uuid.UUID) (bool, error) {
	if _, ok := s.posts[postID]; !ok {
		return false, ErrNotFound
	}
	k := [2]uuid.UUID{userID, postID}
	if _, dup := m[k]; dup {
		return false, nil
	}
	m[k] = s.stamp(time.Time{})
	return true, nil
}

func remove[V any](m map[[2]uuid.UUID]V, a, b uuid.UUID) bool {
	k := [2]uuid.UUID{a, b}
	if _, ok := m[k]; !ok {
		return false
	}
	delete(m, k)
	return true
}

// AddLike implements Store.
func (s *MemStore) AddLike(_ context.Context, userID, postID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, err := s.add(s.likes, userID, postID)
	if ok {
		s.counter(postID, true, 1)
	}
	return ok, err
}

// RemoveLike implements Store.
func (s *MemStore) RemoveLike(_ context.Context, userID, postID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := remove(s.likes, userID, postID)
	if ok {
		s.counter(postID, true, -1)
	}
	return ok, nil
}

// WithdrawnLikes implements Store.
func (s *MemStore) WithdrawnLikes(_ context.Context, likes []LikeKey) (map[LikeKey]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[LikeKey]bool, len(likes))
	for _, l := range likes {
		if _, ok := s.posts[l.PostID]; ok && !has(s.likes, l.UserID, l.PostID) {
			out[l] = true
		}
	}
	return out, nil
}

// AddSave implements Store.
func (s *MemStore) AddSave(_ context.Context, userID, postID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, err := s.add(s.saves, userID, postID)
	if ok {
		s.counter(postID, false, 1)
	}
	return ok, err
}

// RemoveSave implements Store.
func (s *MemStore) RemoveSave(_ context.Context, userID, postID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok := remove(s.saves, userID, postID)
	if ok {
		s.counter(postID, false, -1)
	}
	return ok, nil
}

// AddHide implements Store.
func (s *MemStore) AddHide(_ context.Context, userID, postID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(s.hides, userID, postID)
}

// ListSaved implements Store.
func (s *MemStore) ListSaved(_ context.Context, r Reader, after *After, limit int) ([]SavedPost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SavedPost{}
	if r.UserID == nil {
		return out, nil
	}
	for k, at := range s.saves {
		if k[0] != *r.UserID {
			continue
		}
		p, ok := s.posts[k[1]]
		if !ok || p.Status != StatusPublished || !s.visible(r, p) {
			continue
		}
		if after != nil && cmp.Or(at.Compare(after.At), p.ID.Compare(after.ID)) >= 0 {
			continue
		}
		out = append(out, SavedPost{Post: p.clone(), SavedAt: at})
	}
	slices.SortFunc(out, func(a, b SavedPost) int {
		return -cmp.Or(a.SavedAt.Compare(b.SavedAt), a.Post.ID.Compare(b.Post.ID))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FollowManual implements Store.
func (s *MemStore) FollowManual(_ context.Context, userID, orgID uuid.UUID, notify *bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]uuid.UUID{userID, orgID}
	f, ok := s.follows[k]
	if !ok {
		f = Follow{UserID: userID, OrgID: orgID, CreatedAt: s.stamp(time.Time{})}
	}
	f.Source = FollowManual
	if notify != nil {
		f.Notify = *notify
	}
	s.follows[k] = f
	return !ok, nil
}

// AddAutoFollow implements Store.
func (s *MemStore) AddAutoFollow(_ context.Context, userID, orgID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]uuid.UUID{userID, orgID}
	if _, ok := s.follows[k]; ok {
		return false, nil
	}
	s.follows[k] = Follow{UserID: userID, OrgID: orgID, Source: FollowAutoMembership, CreatedAt: s.stamp(time.Time{})}
	return true, nil
}

// RemoveAutoFollow implements Store.
func (s *MemStore) RemoveAutoFollow(_ context.Context, userID, orgID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]uuid.UUID{userID, orgID}
	if f, ok := s.follows[k]; !ok || f.Source != FollowAutoMembership {
		return false, nil
	}
	delete(s.follows, k)
	return true, nil
}

// RemoveAutoFollowsOf implements Store.
func (s *MemStore) RemoveAutoFollowsOf(_ context.Context, orgID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, f := range s.follows {
		if f.OrgID == orgID && f.Source == FollowAutoMembership {
			delete(s.follows, k)
			n++
		}
	}
	return n, nil
}

// RemoveFollow implements Store.
func (s *MemStore) RemoveFollow(_ context.Context, userID, orgID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return remove(s.follows, userID, orgID), nil
}

// FollowOf returns the follow row of userID and orgID (tests).
func (s *MemStore) FollowOf(userID, orgID uuid.UUID) (Follow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.follows[[2]uuid.UUID{userID, orgID}]
	return f, ok
}

// orgPage cuts a newest-first page of (created_at, org_id) rows.
func orgPage[T any](rows []T, key func(T) (time.Time, uuid.UUID), after *After, limit int) []T {
	if after != nil {
		rows = slices.DeleteFunc(rows, func(r T) bool {
			at, id := key(r)
			return cmp.Or(at.Compare(after.At), id.Compare(after.ID)) >= 0
		})
	}
	slices.SortFunc(rows, func(a, b T) int {
		at, aid := key(a)
		bt, bid := key(b)
		return -cmp.Or(at.Compare(bt), aid.Compare(bid))
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if rows == nil {
		rows = []T{}
	}
	return rows
}

// ListFollows implements Store.
func (s *MemStore) ListFollows(_ context.Context, userID uuid.UUID, after *After, limit int) ([]Follow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []Follow
	for _, f := range s.follows {
		if f.UserID == userID {
			rows = append(rows, f)
		}
	}
	return orgPage(rows, func(f Follow) (time.Time, uuid.UUID) { return f.CreatedAt, f.OrgID }, after, limit), nil
}

// AddMute implements Store.
func (s *MemStore) AddMute(_ context.Context, userID, orgID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]uuid.UUID{userID, orgID}
	if _, ok := s.mutes[k]; ok {
		return false, nil
	}
	s.mutes[k] = s.stamp(time.Time{})
	return true, nil
}

// RemoveMute implements Store.
func (s *MemStore) RemoveMute(_ context.Context, userID, orgID uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return remove(s.mutes, userID, orgID), nil
}

// ListMutes implements Store.
func (s *MemStore) ListMutes(_ context.Context, userID uuid.UUID, after *After, limit int) ([]Mute, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rows []Mute
	for k, at := range s.mutes {
		if k[0] == userID {
			rows = append(rows, Mute{UserID: userID, OrgID: k[1], CreatedAt: at})
		}
	}
	return orgPage(rows, func(m Mute) (time.Time, uuid.UUID) { return m.CreatedAt, m.OrgID }, after, limit), nil
}

// ReconcileCounters implements Store.
func (s *MemStore) ReconcileCounters(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	likes, saves := map[uuid.UUID]int{}, map[uuid.UUID]int{}
	for k := range s.likes {
		likes[k[1]]++
	}
	for k := range s.saves {
		saves[k[1]]++
	}
	fixed := 0
	for id, p := range s.posts {
		if p.LikeCount != likes[id] || p.SaveCount != saves[id] {
			p.LikeCount, p.SaveCount = likes[id], saves[id]
			s.posts[id] = p
			fixed++
		}
	}
	return fixed, nil
}

// InteractionsOf implements Store.
func (s *MemStore) InteractionsOf(_ context.Context, userID uuid.UUID) (Interactions, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	of := func(m map[[2]uuid.UUID]time.Time) []Interaction {
		out := []Interaction{}
		for k, at := range m {
			if k[0] == userID {
				out = append(out, Interaction{PostID: k[1], CreatedAt: at})
			}
		}
		slices.SortFunc(out, func(a, b Interaction) int {
			return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.PostID.Compare(b.PostID))
		})
		return out
	}
	out := Interactions{Likes: of(s.likes), Saves: of(s.saves), Hides: of(s.hides), Follows: []Follow{}, Mutes: []Mute{}}
	for _, f := range s.follows {
		if f.UserID == userID {
			out.Follows = append(out.Follows, f)
		}
	}
	slices.SortFunc(out.Follows, func(a, b Follow) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.OrgID.Compare(b.OrgID))
	})
	for k, at := range s.mutes {
		if k[0] == userID {
			out.Mutes = append(out.Mutes, Mute{UserID: userID, OrgID: k[1], CreatedAt: at})
		}
	}
	slices.SortFunc(out.Mutes, func(a, b Mute) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), a.OrgID.Compare(b.OrgID))
	})
	return out, nil
}

// EraseInteractions implements Store.
func (s *MemStore) EraseInteractions(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k := range s.likes {
		if k[0] == userID {
			delete(s.likes, k)
			s.counter(k[1], true, -1)
			n++
		}
	}
	for k := range s.saves {
		if k[0] == userID {
			delete(s.saves, k)
			s.counter(k[1], false, -1)
			n++
		}
	}
	for _, m := range []map[[2]uuid.UUID]time.Time{s.hides, s.mutes} {
		for k := range m {
			if k[0] == userID {
				delete(m, k)
				n++
			}
		}
	}
	for k, f := range s.follows {
		if f.UserID == userID {
			delete(s.follows, k)
			n++
		}
	}
	return n, nil
}
