package authz

// TypeDirectoryCatalog is the public directory lists (universities, skills,
// clubs): no org, no owner, no Attrs. Declared by the directory policy
// (Policy.Types).
const TypeDirectoryCatalog = "directory_catalog"

// directoryPolicy is the policy of the directory module (03 §5.3). Only
// tickets of the directory module edit this file (BE-001 R5). Written by
// BE-020 for its 12 actions:
//
//	directory.university.list          P (directory_catalog)
//	directory.university.read          P (university); an inactive university
//	directory.department.list            only for platform_admin, else 404
//	directory.skill.list               P (directory_catalog)
//	directory.club.list                P (directory_catalog)
//	directory.club.read                P (club: the catalog is public, 03 §3.6)
//	directory.club.update              S: the club's club_admin; platform_admin
//	                                     (k7 — a sensitive category without the
//	                                     flag — is the service's 422)
//	directory.club.request_sensitivity S: the club's club_admin
//	directory.club.create              platform_admin (admin surface)
//	directory.club.set_sensitivity     platform_admin (admin surface; R in the service)
//	directory.university.manage        platform_admin (admin surface)
//	directory.skill.manage             platform_admin (admin surface)
//
// Resources (03 §3.5): club (OrgID = the club's org; Attrs sensitive,
// university_id), university (OrgID = its org; optional Attr is_active),
// directory_catalog, admin_surface (403 without a platform role, before
// anything is read; 03 §7.2-3). /clubs/{club_id}/... is a resource surface
// (03 §7.6): the club is visible to everyone, so a signed-in user without
// the role gets 403 (not 404).
//
// Sensitivity (03 Y2) concerns membership, not the club's existence: a
// sensitive club is listed and readable like any other; its member count
// is hidden by the service. A club_admin who is a minor is no club_admin
// (Y4, core).
//
// The club claim actions belong to BE-022: until then they are visible and
// granted to no role path (403).
func directoryPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeDirectoryCatalog: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case DirectoryUniversityList, DirectorySkillList, DirectoryClubList:
				return r.Type == TypeDirectoryCatalog
			case DirectoryUniversityRead, DirectoryDepartmentList:
				return r.Type == TypeUniversity && (r.Attr("is_active") != "false" || a.HasPlatformRole(PlatformAdmin))
			case DirectoryClubRead, DirectoryClubUpdate, DirectoryClubRequestSensitivity:
				return r.Type == TypeClub
			case DirectoryClubCreate, DirectoryClubSetSensitivity, DirectoryUniversityManage, DirectorySkillManage:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return true // BE-022's claim actions: no grant below → 403
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			pad := Grant{Role: RolePlatformAdmin}
			clubAdmin := r.OrgID != nil && a.HasOrgRole(*r.OrgID, MemberClubAdmin)
			switch act {
			case DirectoryUniversityList, DirectoryUniversityRead, DirectoryDepartmentList, DirectorySkillList,
				DirectoryClubList, DirectoryClubRead:
				return []Grant{{Role: RolePublic}}
			case DirectoryClubUpdate:
				gs := []Grant{pad}
				if clubAdmin {
					gs = append(gs, Grant{Role: RoleClubAdmin})
				}
				return gs
			case DirectoryClubRequestSensitivity:
				if clubAdmin {
					return []Grant{{Role: RoleClubAdmin}}
				}
				return nil
			case DirectoryClubCreate, DirectoryClubSetSensitivity, DirectoryUniversityManage, DirectorySkillManage:
				return []Grant{pad}
			}
			return nil
		},
	}
}
