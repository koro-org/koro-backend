package identity

import (
	"context"
	"errors"
	"sync"
	"time"
	"uuid"

	"koro/internal/consent"
	"koro/internal/platform/httpx"
)

// The terms gate's check (BE-124, Q-17): httpx.TermsGate asks it on every
// gated write (x-koro-terms-gate: enforced) whether the signed-in user
// accepted the terms of use in force for their audience and locale — the
// answer getMe gives as terms.acceptance_required. A refusal is
// consent.ErrTermsAcceptanceRequired (403); acceptTerms is the way out.
//
// No database round trip per request: the gate keeps, per process,
//
//   - a pass per user: their audience and locale and the version they had
//     accepted when last checked. Acceptances are append-only, so "user
//     accepted version N" never turns false; the pass is good while N is
//     still the version in force (the comparison below) and for at most
//     termsPassTTL (a locale change made through another API instance; the
//     instance that served it forgets the pass at once);
//   - the version in force per (audience, locale), for termsCurrentTTL: a
//     version published through another instance is seen within a minute
//     (the instance that served the publication drops its view at once,
//     TermsPublished). Until then a user who accepted only the previous
//     version may still write: the gate errs on the side of the version
//     the user accepted, never on refusing an acceptance it has not seen.
//
// A refusal is never cached: it is decided from the ledger (the same
// TermsStatus getMe reads), so an acceptance takes effect at once on every
// instance, and the client's retry after acceptTerms passes.
const (
	termsCurrentTTL = time.Minute
	termsPassTTL    = 15 * time.Minute
	// termsPassMax bounds the passes kept: past it the expired ones go and,
	// if none had, all of them (the next writes check again).
	termsPassMax = 100_000
)

// termsKey is the audience and locale a version in force is resolved for.
type termsKey struct {
	aud    consent.Audience
	locale string
}

// termsPass says that its user, a reader of key, had accepted notice when
// it was the version in force (uuid.Nil: none was published then).
type termsPass struct {
	key    termsKey
	notice uuid.UUID
	until  time.Time
}

// termsInForce is the version in force of a key (uuid.Nil: none).
type termsInForce struct {
	notice uuid.UUID
	until  time.Time
}

// termsGate is identity's httpx.TermsChecker (Module.TermsGate).
type termsGate struct {
	s       *service
	mu      sync.Mutex
	passes  map[uuid.UUID]termsPass
	inForce map[termsKey]termsInForce
}

var _ httpx.TermsChecker = (*termsGate)(nil)

func newTermsGate(s *service) *termsGate {
	return &termsGate{s: s, passes: map[uuid.UUID]termsPass{}, inForce: map[termsKey]termsInForce{}}
}

// CheckTerms implements httpx.TermsChecker.
func (g *termsGate) CheckTerms(ctx context.Context, userID uuid.UUID) error {
	now := g.s.now()
	if p, ok := g.pass(userID, now); ok {
		cur, err := g.current(ctx, p.key, now)
		if err != nil {
			return err
		}
		if cur == p.notice {
			return nil
		}
	}
	u, err := g.s.d.Store.User(ctx, userID)
	switch {
	case isNotFound(err):
		return ErrUnauthenticated // erased after the session was resolved
	case err != nil:
		return err
	}
	st, err := g.s.termsState(ctx, u)
	if err != nil {
		return err
	}
	key, notice := termsKey{aud: audienceOf(u.Persona), locale: u.Locale}, uuid.Nil()
	if st.CurrentID != nil {
		notice = *st.CurrentID
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inForce[key] = termsInForce{notice: notice, until: now.Add(termsCurrentTTL)}
	if st.Required {
		delete(g.passes, userID)
		return consent.ErrTermsAcceptanceRequired
	}
	if len(g.passes) >= termsPassMax {
		g.prune(now)
	}
	g.passes[userID] = termsPass{key: key, notice: notice, until: now.Add(termsPassTTL)}
	return nil
}

// pass returns the unexpired pass of userID.
func (g *termsGate) pass(userID uuid.UUID, now time.Time) (termsPass, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	p, ok := g.passes[userID]
	if !ok || !now.Before(p.until) {
		return termsPass{}, false
	}
	return p, true
}

// current returns the version in force of key: the cached one while it is
// fresh, otherwise consent's (one query, cached for termsCurrentTTL).
func (g *termsGate) current(ctx context.Context, key termsKey, now time.Time) (uuid.UUID, error) {
	g.mu.Lock()
	c, ok := g.inForce[key]
	g.mu.Unlock()
	if ok && now.Before(c.until) {
		return c.notice, nil
	}
	n, err := g.s.d.Terms.Current(ctx, consent.NoticeKey{Kind: consent.KindTermsOfUse, Audience: key.aud, Locale: key.locale})
	id := n.ID
	switch {
	case errors.Is(err, consent.ErrNoNotice):
		id = uuid.Nil()
	case err != nil:
		return uuid.Nil(), err
	}
	g.mu.Lock()
	g.inForce[key] = termsInForce{notice: id, until: now.Add(termsCurrentTTL)}
	g.mu.Unlock()
	return id, nil
}

// prune drops the expired passes, or every pass when none has expired.
// g.mu is held.
func (g *termsGate) prune(now time.Time) {
	for id, p := range g.passes {
		if !now.Before(p.until) {
			delete(g.passes, id)
		}
	}
	if len(g.passes) >= termsPassMax {
		clear(g.passes)
	}
}

// forget drops the pass of userID (its locale changed: the version in
// force may differ).
func (g *termsGate) forget(userID uuid.UUID) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.passes, userID)
}

// dropInForce forgets every cached version in force (a terms of use
// version was published through this process).
func (g *termsGate) dropInForce() {
	g.mu.Lock()
	defer g.mu.Unlock()
	clear(g.inForce)
}
