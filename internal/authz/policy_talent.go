package authz

// talentPolicy is the policy of the talent module (03 §5.12). Only tickets of
// the talent module edit this file (BE-001 R5).
//
// BE-047 (the ledger, no HTTP surface): the talent consumers act as
// SystemActor(<subscription>) on talent.ledger.write, the nightly score
// (BE-052) on talent.score.compute; both are on the system allow-list of
// 03 §3.7, which the core decides before any module policy
// (TestTalentPolicy_SystemActionsOnly). The HTTP actions stay default deny
// for every role until BE-052 writes Visible and Grants for the
// talent_auditor endpoints (k-flag, Y1: never platform_admin).
func talentPolicy() Policy { return Policy{} }
