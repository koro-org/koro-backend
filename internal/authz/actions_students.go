// Action constants transcribed from docs/design/03-yetki-matrisi.md by BE-004 (BE-001 R5).
// Module tickets never edit this file: a new action is added to 03 first (orchestrator),
// then here. TestActions_CountMatchesDesign and TestActions_MatchDesignDoc pin the content.

package authz

// HTTP actions of the students module (03 §5.4), in table order.
const (
	StudentsProfileReadOwn         Action = "students.profile.read_own"
	StudentsProfileUpdateOwn       Action = "students.profile.update_own"
	StudentsProfileSectionManage   Action = "students.profile_section.manage"
	StudentsCvManage               Action = "students.cv.manage"
	StudentsCvReadOwn              Action = "students.cv.read_own"
	StudentsPrivacyReadOwn         Action = "students.privacy.read_own"
	StudentsPrivacyUpdateOwn       Action = "students.privacy.update_own"
	StudentsAccessGrantListOwn     Action = "students.access_grant.list_own"
	StudentsAccessGrantRevoke      Action = "students.access_grant.revoke"
	StudentsAccessGrantListCompany Action = "students.access_grant.list_company"
	StudentsProfileReadCompany     Action = "students.profile.read_company"
	StudentsCvReadCompany          Action = "students.cv.read_company"
	StudentsProfileReadMemberCard  Action = "students.profile.read_member_card"
	StudentsProfileReadAdmin       Action = "students.profile.read_admin"
)

// studentsActions lists the HTTP actions of students in 03 §5.4 order.
var studentsActions = []Action{
	StudentsProfileReadOwn,
	StudentsProfileUpdateOwn,
	StudentsProfileSectionManage,
	StudentsCvManage,
	StudentsCvReadOwn,
	StudentsPrivacyReadOwn,
	StudentsPrivacyUpdateOwn,
	StudentsAccessGrantListOwn,
	StudentsAccessGrantRevoke,
	StudentsAccessGrantListCompany,
	StudentsProfileReadCompany,
	StudentsCvReadCompany,
	StudentsProfileReadMemberCard,
	StudentsProfileReadAdmin,
}
