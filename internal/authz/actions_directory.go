// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the directory module (03 §5.3), in table order.
const (
	DirectoryUniversityList         Action = "directory.university.list"
	DirectoryUniversityRead         Action = "directory.university.read"
	DirectoryDepartmentList         Action = "directory.department.list"
	DirectorySkillList              Action = "directory.skill.list"
	DirectoryClubList               Action = "directory.club.list"
	DirectoryClubRead               Action = "directory.club.read"
	DirectoryClubUpdate             Action = "directory.club.update"
	DirectoryClubCreate             Action = "directory.club.create"
	DirectoryClubSetSensitivity     Action = "directory.club.set_sensitivity"
	DirectoryClubRequestSensitivity Action = "directory.club.request_sensitivity"
	DirectoryClubClaimCreate        Action = "directory.club_claim.create"
	DirectoryClubClaimReadOwn       Action = "directory.club_claim.read_own"
	DirectoryClubClaimWithdraw      Action = "directory.club_claim.withdraw"
	DirectoryClubClaimList          Action = "directory.club_claim.list"
	DirectoryClubClaimDecide        Action = "directory.club_claim.decide"
	DirectoryUniversityManage       Action = "directory.university.manage"
	DirectorySkillManage            Action = "directory.skill.manage"
)

// directoryActions lists the HTTP actions of directory in 03 §5.3 order.
var directoryActions = []Action{
	DirectoryUniversityList,
	DirectoryUniversityRead,
	DirectoryDepartmentList,
	DirectorySkillList,
	DirectoryClubList,
	DirectoryClubRead,
	DirectoryClubUpdate,
	DirectoryClubCreate,
	DirectoryClubSetSensitivity,
	DirectoryClubRequestSensitivity,
	DirectoryClubClaimCreate,
	DirectoryClubClaimReadOwn,
	DirectoryClubClaimWithdraw,
	DirectoryClubClaimList,
	DirectoryClubClaimDecide,
	DirectoryUniversityManage,
	DirectorySkillManage,
}
