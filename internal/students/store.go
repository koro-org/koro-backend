package students

// Store is the persistence boundary of students (05 §2.2) over
// student_profiles, student_privacy_settings, student_skills,
// student_experiences, student_languages, student_cvs and
// profile_access_grants (02 §9.1–9.7) and the count_segment function
// (02 §9.1, 01 §1.4). The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any. Org scope:
// none (02 §9: a student belongs to no org) but for the access grants
// (company_org_id); every per-user table is
// addressed by user_id, and an experience is read and written only
// together with its owner's user_id (IDOR: another user's ID is not found).
//
// Store is split by aggregate into embedded interfaces (05 §2.2, BE-142);
// a service file reaches the store only through the narrow view of the
// aggregate it works on (the accessors below), the KVKK provider
// (privacy.go), which covers every table, through the whole Store.
type Store interface {
	profileStore // student_profiles and the audience columns (store_profile.go)
	privacyStore // student_privacy_settings (store_privacy.go)
	sectionStore // skills, languages and experiences (store_sections.go)
	cvStore      // student_cvs (store_cv.go)
	matchStore   // count_segment and the discovery candidates (store_match.go)
	grantStore   // profile_access_grants and their contact sharing (store_grants.go)
	statsStore   // the counts of a university's panel (stats.go, BE-163)
}

// stats is the university panel count view of the store (BE-163).
func (s *service) stats() statsStore { return s.d.Store }

// profiles is the profile view of the store.
func (s *service) profiles() profileStore { return s.d.Store }

// privacy is the privacy settings view of the store.
func (s *service) privacy() privacyStore { return s.d.Store }

// sections is the skills, languages and experiences view of the store.
func (s *service) sections() sectionStore { return s.d.Store }

// cvs is the CV view of the store.
func (s *service) cvs() cvStore { return s.d.Store }

// matches is the segment and discovery view of the store.
func (s *service) matches() matchStore { return s.d.Store }

// grants is the access grant view of the store.
func (s *service) grants() grantStore { return s.d.Store }
