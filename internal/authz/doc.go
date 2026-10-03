// Package authz is Koro's authorization core (docs/design/03): the Actor and
// Resource types, every action constant (03 §5, §3.7), one policy per module
// (policy_<module>.go) and Can, the decision every HTTP-facing service method
// makes first (05 §5.3).
//
// authz is a leaf (01 §3.1): it performs no I/O and imports no feature
// module. The calling service reads the facts and passes them in Resource
// (03 §3.3, §3.5).
//
// Decision order (03 §3.2), implemented once in can.go for every module:
//
//  1. Unknown action, unknown resource type or a missing required Attrs key:
//     ErrForbidden (programmer error, see Denial).
//  2. System actor: the 03 §3.7 allow-list only. System-only actions are
//     never granted to any other actor.
//  3. Anonymous actor and no public grant: ErrForbidden.
//  4. Resource not visible to the actor (Policy.Visible, 03 §3.6): ErrNotFound.
//  5. Absolute bans (03 §3.4; absolute.go), applied after every role path:
//     talent, sensitive club, minor club_admin, company access to student
//     data, self-approval. ErrForbidden or ErrNotFound by rule.
//  6. No grant left: ErrForbidden.
//  7. Per grant: MFA, then four eyes, then the break-glass reason. The first
//     grant that satisfies all of them allows (nil); otherwise the earliest
//     failing condition in that order is returned.
//
// Default deny: a module whose policy is not written yet (the stub in
// policy_<module>.go) denies every action.
package authz
