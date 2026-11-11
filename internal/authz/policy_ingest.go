package authz

// TypeIngestCatalog is the public imported events (BE-210:
// ingest.imported_event.list/read/request_removal): no org, no Attrs.
// Declared by the ingest policy.
const TypeIngestCatalog = "ingest_catalog"

// ingestPolicy is the policy of the ingest module (03 §5.25 doc patch;
// BE-210). Only tickets of the ingest module edit this file (BE-001 R5).
//
//	ingest.imported_event.list             P (everyone, anonymous included)
//	ingest.imported_event.read             P
//	ingest.imported_event.request_removal  P: anyone may ask that an imported event be removed
//	                                       (the subject of the page may have no account); it is
//	                                       hidden at once and waits for a platform admin
//	ingest.suggestion.list                 S: club_admin of the club; platform_admin
//	ingest.suggestion.promote              S: club_admin of the club only (the draft is the
//	                                       club's own, events.event.create decides again)
//	ingest.suggestion.reject               S: club_admin of the club only
//	ingest.source.*, ingest.imported_event.list_admin/moderate/delete,
//	ingest.imported_club.list/review       platform_admin (admin surface)
//
// Resources: the public catalog is TypeIngestCatalog; the suggestions pass
// the TypeOrg resource of the path's club (org_kind, org_status), visible
// to the club's active members (a member without the role gets 403) and to
// platform staff (403 unless platform_admin), 404 for everyone else (03
// §3.6); the admin operations the admin surface (403 without the role,
// 03 §7.2-3). MFA follows from the role paths (k-mfa: pad).
func ingestPolicy() Policy {
	return Policy{
		Types: map[string][]string{TypeIngestCatalog: nil},
		Visible: func(a Actor, act Action, r Resource) bool {
			switch act {
			case IngestImportedEventList, IngestImportedEventRead, IngestImportedEventRequestRemoval:
				return r.Type == TypeIngestCatalog
			case IngestSuggestionList, IngestSuggestionPromote, IngestSuggestionReject:
				return r.Type == TypeOrg && OrgSurfaceVisible(a, r.OrgID)
			case IngestSourceList, IngestSourceRead, IngestSourceCreate, IngestSourceUpdate, IngestSourceRun,
				IngestImportedEventListAdmin, IngestImportedEventModerate, IngestImportedEventDelete,
				IngestImportedClubList, IngestImportedClubReview:
				return r.Type == TypeAdminSurface && AdminSurfaceVisible(a)
			}
			return false
		},
		Grants: func(a Actor, act Action, r Resource) []Grant {
			clubAdmin := r.OrgID != nil && r.Attr("org_kind") == "club" && a.HasOrgRole(*r.OrgID, MemberClubAdmin)
			switch act {
			case IngestImportedEventList, IngestImportedEventRead, IngestImportedEventRequestRemoval:
				return []Grant{{Role: RolePublic}}
			case IngestSuggestionList:
				gs := []Grant{{Role: RolePlatformAdmin}}
				if clubAdmin {
					gs = append(gs, Grant{Role: RoleClubAdmin})
				}
				return gs
			case IngestSuggestionPromote, IngestSuggestionReject:
				if clubAdmin {
					return []Grant{{Role: RoleClubAdmin}}
				}
				return nil
			case IngestSourceList, IngestSourceRead, IngestSourceCreate, IngestSourceUpdate, IngestSourceRun,
				IngestImportedEventListAdmin, IngestImportedEventModerate, IngestImportedEventDelete,
				IngestImportedClubList, IngestImportedClubReview:
				return []Grant{{Role: RolePlatformAdmin}}
			}
			return nil
		},
	}
}
