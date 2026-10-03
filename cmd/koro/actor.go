package main

import (
	"context"
	"errors"
	"fmt"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/httpx"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
)

// actorResolver is the production httpx.ActorResolver (03 §2.2, 05 §5.2;
// BE-014). Per request, without a cache (a revoked role or session is
// effective at once):
//
//  1. identity authenticates the credential: live session, matching
//     transport (web → cookie, mobile → bearer), active user, sliding idle
//     window; platform roles in effect only (06 H-33) and the org
//     memberships of hooks.memberships (orgs, BE-019; none until then);
//  2. the student verification (identity.StudentVerification; BE-018
//     replaces the stub) gives StudentStatus and UniversityID;
//  3. IsMinor is computed from the birth date at this request's instant
//     (02 §5.1: never stored).
//
// Every rejection is the same problem.ErrUnauthenticated (the middleware
// turns it into 401 on protected operations and into an anonymous actor on
// public ones, 06 H-25); any other error is a server failure.
type actorResolver struct {
	auth     identity.Authenticator
	students identity.StudentVerification
	clock    clock.Clock
}

var _ httpx.ActorResolver = actorResolver{}

func (r actorResolver) Resolve(ctx context.Context, cred httpx.Credential) (context.Context, error) {
	a, err := r.actor(ctx, cred)
	if err != nil {
		return nil, err
	}
	return obs.WithActorID(authz.WithActor(ctx, a), a.UserID.String()), nil
}

func (r actorResolver) actor(ctx context.Context, cred httpx.Credential) (authz.Actor, error) {
	p, err := r.auth.Authenticate(ctx, cred)
	switch {
	case errors.Is(err, problem.ErrUnauthenticated):
		return authz.Actor{}, problem.ErrUnauthenticated
	case err != nil:
		return authz.Actor{}, fmt.Errorf("actor: authenticate: %w", err)
	}
	a := authz.Actor{
		Kind: authz.KindUser, UserID: p.UserID, SessionID: p.SessionID,
		PlatformRoles: p.PlatformRoles, Memberships: p.Memberships,
		StudentStatus: authz.StudentNone, MFAVerified: p.MFAVerified,
		IsMinor: p.BirthDate != nil && p.BirthDate.IsMinor(r.clock.Now()),
	}
	v, ok, err := r.students.Active(ctx, p.UserID)
	if err != nil {
		return authz.Actor{}, fmt.Errorf("actor: student verification: %w", err)
	}
	if ok {
		uni := v.UniversityID
		a.StudentStatus, a.UniversityID = authz.StudentVerified, &uni
	}
	return a, nil
}
