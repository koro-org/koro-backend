// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the talent module (03 §5.12), in table order.
const (
	TalentReportRead           Action = "talent.report.read"
	TalentDistributionRead     Action = "talent.distribution.read"
	TalentNeutralizationManage Action = "talent.neutralization.manage"
	TalentReversalCreate       Action = "talent.reversal.create"
	TalentConfigRead           Action = "talent.config.read"
	TalentConfigManage         Action = "talent.config.manage"
)

// System-only actions of the talent module (03 §3.7): only SystemActor may perform them.
const (
	TalentLedgerWrite  Action = "talent.ledger.write"
	TalentScoreCompute Action = "talent.score.compute"
)

// talentActions lists the HTTP actions of talent in 03 §5.12 order.
var talentActions = []Action{
	TalentReportRead,
	TalentDistributionRead,
	TalentNeutralizationManage,
	TalentReversalCreate,
	TalentConfigRead,
	TalentConfigManage,
}
