package identity

import (
	"context"
	"errors"
	"uuid"

	"koro/internal/authz"
)

// Student verification at sign-up (BE-097, koro-mobile B-06).
//
// A student who registers with a .edu.tr address proves that address once:
// the account_email token of the registration also completes the student
// verification of 01 §4.6 when it is confirmed. There is no second e-mail
// and no second token; the rules are those of confirmEduVerification
// (completeEduEmail: one year, supersede, user.edu_verified with the same
// payload, the same audit action).
//
//   - The binding is made when the account and its first token are written:
//     the token's target_email holds the address the token proves for the
//     student verification (02 §5.3 allows it for account_email). Only the
//     token issued with a new student account carries it. A token re-issued
//     for the pending account (resendEmailVerification, or another
//     registration with the same address) closes it and proves the account
//     address only: after a second registration the password may not be
//     the clicker's, and the student role must not ride on such a click
//     (account pre-hijacking). The student then uses the normal path.
//   - The university is resolved when the token is confirmed (the directory
//     list of the moment, like confirmEduVerification); registration does
//     no lookup, so its answer and cost stay the same for every address.
//   - The student verification is written only when the confirmation
//     activates the account (pending_email → active), in the same
//     transaction. When the domain is not a known university, or the
//     address already verifies another account, the account alone is
//     verified: the answer is the same 204 (the confirmer learns nothing
//     about other accounts), the audit keeps the reason.
const (
	signupEduVerified      = "verified"
	signupEduDomainUnknown = "domain_unknown"
	signupEduAddressTaken  = "address_taken"
	signupEduIneligible    = "ineligible"
)

// signupEduTarget is the target_email of the first account_email token of
// a new account: a student's .edu.tr address, "" for everybody else.
func signupEduTarget(u User) string {
	if u.Persona != PersonaStudent {
		return ""
	}
	if _, _, _, err := parseEduEmail(u.Email, "body.email"); err != nil {
		return ""
	}
	return u.Email
}

// signupEdu is the student verification a confirmed account_email token
// carries: addr when it can be completed, otherwise the reason it is
// skipped ("" when the token carries none).
type signupEdu struct {
	addr *eduAddress
	skip string
}

// signupEduOf resolves the student verification of token t of account u
// before the transaction (the domain lookup is a read).
func (s *service) signupEduOf(ctx context.Context, a authz.Actor, t EmailToken, u User) (signupEdu, error) {
	if t.TargetEmail == "" || u.Status != StatusPendingEmail {
		return signupEdu{}, nil
	}
	// A target that is no longer this student's .edu.tr address, or a
	// caller the student confirmation refuses, verifies the account alone.
	addr, ok := signupAddress(t, u)
	if !ok || !eduConfirmAllowed(ctx, a) {
		return signupEdu{skip: signupEduIneligible}, nil
	}
	uni, err := s.resolveDomain(ctx, addr.domain, "body.token")
	switch {
	case errors.Is(err, ErrEduDomainUnknown):
		return signupEdu{skip: signupEduDomainUnknown}, nil
	case err != nil:
		return signupEdu{}, err
	}
	addr.uni = uni
	return signupEdu{addr: &addr}, nil
}

// eduConfirmAllowed applies the decision of confirmEduVerification (a
// public action) to the student step of the caller.
func eduConfirmAllowed(ctx context.Context, a authz.Actor) bool {
	return authz.Can(ctx, a, authz.IdentityEduVerificationConfirm, publicUser) == nil
}

// signupAddress is the student .edu.tr address token t proves for account
// u: its target, when u is a student whose address it still is.
func signupAddress(t EmailToken, u User) (eduAddress, bool) {
	email, normalized, domain, err := parseEduEmail(t.TargetEmail, "body.token")
	if err != nil || u.Persona != PersonaStudent || normalized != u.EmailNormalized {
		return eduAddress{}, false
	}
	return eduAddress{email: email, normalized: normalized, domain: domain}, true
}

// completeSignupEdu writes the student verification of cur (just activated
// and locked in the caller's transaction). It returns the audit result and
// the new row (nil when skipped).
func (s *service) completeSignupEdu(ctx context.Context, cur User, tokenID uuid.UUID, e signupEdu) (string, *EduRecord, error) {
	if e.addr == nil {
		return e.skip, nil, nil
	}
	if cur.Persona != PersonaStudent || cur.EmailNormalized != e.addr.normalized {
		return signupEduIneligible, nil, nil
	}
	holder, taken, err := s.d.Store.OpenEduVerificationOfEmail(ctx, e.addr.normalized)
	switch {
	case err != nil:
		return "", nil, err
	case taken && holder.UserID != cur.ID:
		return signupEduAddressTaken, nil, nil
	}
	v, err := s.completeEduEmail(ctx, cur, tokenID, *e.addr, map[string]any{"via": string(PurposeAccountEmail)})
	if err != nil {
		return "", nil, err
	}
	return signupEduVerified, &v, nil
}
