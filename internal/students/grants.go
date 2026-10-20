package students

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/directory"
	"koro/internal/files"
	"koro/internal/identity"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
	"koro/internal/platform/textx"
)

// Level 3 access grants (02 §9.7, 03 §5.4, 04 §2.4, §V.3; BE-046).
const (
	// companyCVTTL is the lifetime of a company's CV download URL: shorter
	// than the student's own (04 §1.14 allows ≤ 5 minutes) so a URL a
	// company fetched just before the student revoked expires within a
	// minute.
	companyCVTTL = time.Minute
	// maxRevokeReason is profile_access_grants.revoke_reason's limit.
	maxRevokeReason = 300
	// maxStudentQuery is the longest name filter of listCompanyGrantedStudents.
	maxStudentQuery = 100
	// companyScanLimit is the most grants one company list request
	// inspects; a sparse name filter answers a short page with a cursor.
	companyScanLimit = 1000
	// grantBatch is how many grants a list reads per store call.
	grantBatch = 100

	// grantJob is the system actor of AccessGrants.Revoke (another module
	// ends a grant).
	grantJob = "students.access_grants"
	// auditGrantCreate is the audit action of AccessGrants.Grant (the
	// student's acceptance, written in messaging's transaction; 03 §4.1
	// format, not an HTTP action).
	auditGrantCreate = "students.access_grant.create"
)

// GrantView is one of the student's grants with the company's name
// (listMyAccessGrants).
type GrantView struct {
	Grant       AccessGrant
	CompanyName string
}

// GrantPage is a page of the student's active grants; Next is the position
// after the last item when there is another page.
type GrantPage struct {
	Items []GrantView
	Next  *GrantKey
}

// CompanyStudentQuery narrows listCompanyGrantedStudents: Q filters by the
// student's name (case and Turkish letters folded), After is the keyset
// position (granted_at DESC, id DESC), Limit the page size (1–100).
type CompanyStudentQuery struct {
	Q     string
	After *GrantKey
	Limit int
}

// CompanyStudentPage is a page of a company's granted students.
type CompanyStudentPage struct {
	Items []StudentL3CardDTO
	Next  *GrantKey
}

// companyTarget is what a company view of a student decides on, read
// before authz: the company's active grant and whether the student is one
// a company may see at all (a live adult account with a stored profile,
// 04 §V.0-3). visible is false otherwise, and the view is then the same
// 404 as for an unknown student.
type companyTarget struct {
	grant   AccessGrant
	profile Profile
	user    identity.UserSummary
	visible bool
}

// accessAttr is the Attrs access_grant of t (03 §3.5).
func (t companyTarget) accessAttr() string {
	if t.visible {
		return "active"
	}
	return "none"
}

func (s *service) companyTarget(ctx context.Context, orgID, userID uuid.UUID) (companyTarget, error) {
	var t companyTarget
	if orgID == uuid.Nil() || userID == uuid.Nil() {
		return t, nil
	}
	g, err := s.d.Store.ActiveGrant(ctx, userID, orgID)
	switch {
	case errors.Is(err, ErrNotFound):
		return t, nil
	case err != nil:
		return t, err
	}
	users, err := s.d.Users.GetMany(ctx, []uuid.UUID{userID})
	if err != nil {
		return t, err
	}
	u, ok := users[userID]
	if !ok || u.Status != identity.StatusActive || u.IsMinor {
		return t, nil
	}
	p, err := s.d.Store.GetProfile(ctx, userID)
	switch {
	case errors.Is(err, ErrNotFound):
		return t, nil
	case err != nil:
		return t, err
	}
	return companyTarget{grant: g, profile: p, user: u, visible: true}, nil
}

// companyResource is the authz view of a company's request about a student
// (03 §3.5; BE-046): OrgID = the company of the path, OwnerUserID = the
// student, the access grant the service read (Y5 hides the student from
// company roles without "active").
func companyResource(typ string, orgID, userID uuid.UUID, t companyTarget) authz.Resource {
	org, user := orgID, userID
	r := authz.Resource{Type: typ, ID: user, OrgID: &org, OwnerUserID: &user}
	switch typ {
	case authz.TypeStudentProfile:
		minor := ""
		if t.visible {
			minor = strconv.FormatBool(t.user.IsMinor)
		}
		r.Attrs = map[string]string{authz.AttrAccessGrant: t.accessAttr(), "discoverable": "", "minor": minor}
	case authz.TypeStudentCV:
		r.Attrs = map[string]string{authz.AttrAccessGrant: t.accessAttr()}
	}
	return r
}

// ownGrantResource is the authz view of the caller's own grants (the /me
// path: no OrgID; 06 H-27 OwnerUserID always set). id is one grant or
// uuid.Nil() for the list.
func ownGrantResource(a authz.Actor, id uuid.UUID) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: authz.TypeAccessGrant, ID: id, OwnerUserID: &me}
}

// canOwnGrants checks an own-grant action (k-uv: 403
// students.verification_required).
func (s *service) canOwnGrants(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID) error {
	err := authz.Can(ctx, a, act, ownGrantResource(a, id))
	if err != nil && errors.Is(err, authz.ErrForbidden) && a.IsUser() && !a.IsVerifiedStudent() {
		return ErrVerificationRequired
	}
	return err
}

// cleanReason validates an optional revocation reason (≤ 300 characters,
// one line); "" is none.
func cleanReason(reason string) (*string, error) {
	return cleanLine("body.reason", reason, maxRevokeReason)
}

// grantEvent is the audit record of a grant write or read.
func grantEvent(action string, g AccessGrant, meta map[string]any) audit.Event {
	id, org := g.ID, g.CompanyOrgID
	if meta == nil {
		meta = map[string]any{}
	}
	meta[audit.MetaSubjectUserID] = g.StudentUserID.String()
	return audit.Event{Action: action, Outcome: audit.Success, OrgID: &org, ResourceType: authz.TypeAccessGrant,
		ResourceID: &id, Metadata: meta}
}

// ---- AccessGrants (01 §4.9)

// Grant implements AccessGrants. Order: the arguments → the account (an
// unknown or inactive one is not found, a minor 422) → in the caller's
// transaction: the student's grant lock, a replay of the same source (no
// change; the source naming another student or company is a conflict), an
// active grant of the company (no change), the new row and its audit
// record (the student is the actor: the acceptance is theirs).
func (s *service) Grant(ctx context.Context, studentID, companyOrgID uuid.UUID, src GrantSource, srcID uuid.UUID) error {
	if studentID == uuid.Nil() || companyOrgID == uuid.Nil() || srcID == uuid.Nil() || !src.Valid() {
		return problem.ErrValidation.Wrap(errors.New("students: invalid access grant"))
	}
	users, err := s.d.Users.GetMany(ctx, []uuid.UUID{studentID})
	if err != nil {
		return err
	}
	u, ok := users[studentID]
	switch {
	case !ok || u.Status != identity.StatusActive:
		return ErrNotFound
	case u.IsMinor: // 04 §V.0-3: a minor never reaches a company view
		return ErrMinorRestricted
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.LockGrants(ctx, studentID); err != nil {
			return err
		}
		switch g, err := s.d.Store.GrantBySource(ctx, src, srcID); {
		case err == nil:
			if g.StudentUserID != studentID || g.CompanyOrgID != companyOrgID {
				return problem.ErrConflict
			}
			return nil // a replay; a revoked grant is not given again by its old request
		case !errors.Is(err, ErrNotFound):
			return err
		}
		switch _, err := s.d.Store.ActiveGrant(ctx, studentID, companyOrgID); {
		case err == nil:
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}
		g, ok, err := s.d.Store.InsertGrant(ctx, AccessGrant{
			ID: uuid.NewV7(), StudentUserID: studentID, CompanyOrgID: companyOrgID, Source: src, SourceID: srcID,
			GrantedAt: s.now(),
		})
		if err != nil {
			return err
		}
		if !ok {
			return problem.ErrConflict
		}
		ev := grantEvent(auditGrantCreate, g, map[string]any{"source": string(src), "source_id": srcID.String()})
		student := studentID
		ev.ActorUserID = &student
		return s.d.Audit.Log(ctx, ev)
	})
}

// Revoke implements AccessGrants: the grant of source srcID, when it is
// the pair's and still active, is revoked by the system (audited). The
// reason is stored, never shown to the student or the company.
func (s *service) Revoke(ctx context.Context, studentID, companyOrgID uuid.UUID, src GrantSource, srcID uuid.UUID, reason string) error {
	why, err := cleanReason(reason)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.LockGrants(ctx, studentID); err != nil {
			return err
		}
		g, err := s.d.Store.GrantBySource(ctx, src, srcID)
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		if g.StudentUserID != studentID || g.CompanyOrgID != companyOrgID || !g.Active() {
			return nil
		}
		revoked, err := s.d.Store.RevokeGrants(ctx, studentID, companyOrgID, RevokedBySystem, why, s.now())
		if err != nil {
			return err
		}
		return s.auditRevocations(ctx, authz.SystemActor(grantJob), revoked, RevokedBySystem)
	})
}

// Has implements AccessGrants.
func (s *service) Has(ctx context.Context, studentID, companyOrgID uuid.UUID) (bool, error) {
	_, err := s.d.Store.ActiveGrant(ctx, studentID, companyOrgID)
	switch {
	case errors.Is(err, ErrNotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// auditRevocations writes one record per revoked grant (no free text: the
// reason stays in the row).
func (s *service) auditRevocations(ctx context.Context, by authz.Actor, revoked []AccessGrant, kind RevokedBy) error {
	for _, g := range revoked {
		ev := grantEvent(string(authz.StudentsAccessGrantRevoke), g, map[string]any{"revoked_by": string(kind)})
		if kind == RevokedByCompany {
			org := g.CompanyOrgID
			ev.ActorOrgID = &org
		}
		if err := s.d.Audit.Log(ctx, ev.By(by)); err != nil {
			return err
		}
	}
	return nil
}

// ---- the student's side

// ListMyAccessGrants implements Service: the caller's active grants,
// newest first, with the company names (orgs).
func (s *service) ListMyAccessGrants(ctx context.Context, a authz.Actor, after *GrantKey, limit int) (GrantPage, error) {
	if err := s.canOwnGrants(ctx, a, authz.StudentsAccessGrantListOwn, uuid.Nil()); err != nil {
		return GrantPage{}, err
	}
	limit = page.ClampLimit(limit)
	rows, err := s.d.Store.StudentGrants(ctx, a.UserID, after, limit+1)
	if err != nil {
		return GrantPage{}, err
	}
	var out GrantPage
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[limit-1]
		out.Next = &GrantKey{GrantedAt: last.GrantedAt, ID: last.ID}
	}
	ids := make([]uuid.UUID, len(rows))
	for i, g := range rows {
		ids[i] = g.CompanyOrgID
	}
	names, err := s.orgNames(ctx, ids)
	if err != nil {
		return GrantPage{}, err
	}
	out.Items = make([]GrantView, len(rows))
	for i, g := range rows {
		out.Items[i] = GrantView{Grant: g, CompanyName: names[g.CompanyOrgID]}
	}
	return out, nil
}

// orgNames returns the names of the orgs among ids that exist (none
// without OrgNames).
func (s *service) orgNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if s.d.OrgNames == nil || len(ids) == 0 {
		return out, nil
	}
	uniq := slices.Clone(ids)
	slices.SortFunc(uniq, compareUUID)
	sums, err := s.d.OrgNames.GetMany(ctx, slices.Compact(uniq))
	if err != nil {
		return nil, err
	}
	for id, o := range sums {
		out[id] = o.Name
	}
	return out, nil
}

// RevokeMyAccessGrant implements Service (students.access_grant.revoke,
// the student's path). Order: authz (k-uv 403) → the reason (422) → in one
// transaction: the student's grant lock, the grant (another student's or
// an unknown one: 404; an already revoked one: done, nothing written),
// every active grant of that company revoked (by the student) and audited.
// The company's next read fails: every company view checks the grant.
func (s *service) RevokeMyAccessGrant(ctx context.Context, a authz.Actor, grantID uuid.UUID, reason string) error {
	if err := s.canOwnGrants(ctx, a, authz.StudentsAccessGrantRevoke, grantID); err != nil {
		return err
	}
	why, err := cleanReason(reason)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.LockGrants(ctx, a.UserID); err != nil {
			return err
		}
		g, err := s.d.Store.GetGrant(ctx, a.UserID, grantID)
		if err != nil {
			return err
		}
		if !g.Active() {
			return nil
		}
		revoked, err := s.d.Store.RevokeGrants(ctx, a.UserID, g.CompanyOrgID, RevokedByStudent, why, s.now())
		if err != nil {
			return err
		}
		return s.auditRevocations(ctx, a, revoked, RevokedByStudent)
	})
}

// ---- the company's side

// RevokeCompanyAccessGrant implements Service (students.access_grant.revoke,
// the company's path). Order: authz on the company's surface (another
// company 404, staff 403) → the reason → in one transaction: the student's
// grant lock, every active grant of the company on the student revoked (by
// the company; none: 404) and audited.
func (s *service) RevokeCompanyAccessGrant(ctx context.Context, a authz.Actor, orgID, userID uuid.UUID, reason string) error {
	if err := authz.Can(ctx, a, authz.StudentsAccessGrantRevoke,
		companyResource(authz.TypeAccessGrant, orgID, userID, companyTarget{})); err != nil {
		return err
	}
	why, err := cleanReason(reason)
	if err != nil {
		return err
	}
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		if err := s.d.Store.LockGrants(ctx, userID); err != nil {
			return err
		}
		revoked, err := s.d.Store.RevokeGrants(ctx, userID, orgID, RevokedByCompany, why, s.now())
		if err != nil {
			return err
		}
		if len(revoked) == 0 {
			return ErrNotFound
		}
		return s.auditRevocations(ctx, a, revoked, RevokedByCompany)
	})
}

// ListCompanyStudents implements Service (students.access_grant.list_company):
// the company's active grants, newest first, as level 3 cards of the
// students a company may see (no minor, no inactive account, no missing
// profile), optionally filtered by name. One request inspects at most
// companyScanLimit grants; a sparse filter then answers a short page with
// a cursor.
func (s *service) ListCompanyStudents(ctx context.Context, a authz.Actor, orgID uuid.UUID, q CompanyStudentQuery) (CompanyStudentPage, error) {
	org := orgID
	if err := authz.Can(ctx, a, authz.StudentsAccessGrantListCompany,
		authz.Resource{Type: authz.TypeAccessGrant, OrgID: &org}); err != nil {
		return CompanyStudentPage{}, err
	}
	needle, err := cleanLine("query.q", q.Q, maxStudentQuery)
	if err != nil {
		return CompanyStudentPage{}, err
	}
	match := func(first, last string) bool {
		return needle == nil || strings.Contains(textx.LowerTR(first+" "+last), textx.LowerTR(*needle))
	}
	limit := page.ClampLimit(q.Limit)
	type card struct {
		dto StudentL3CardDTO
		key GrantKey
	}
	var cards []card
	pos, scanned, exhausted := q.After, 0, false
	for len(cards) <= limit && scanned < companyScanLimit {
		rows, err := s.d.Store.CompanyGrants(ctx, orgID, pos, grantBatch)
		if err != nil {
			return CompanyStudentPage{}, err
		}
		if len(rows) < grantBatch {
			exhausted = true
		}
		srcs, err := s.cardSources(ctx, rows)
		if err != nil {
			return CompanyStudentPage{}, err
		}
		for _, g := range rows {
			scanned++
			pos = &GrantKey{GrantedAt: g.GrantedAt, ID: g.ID}
			src, ok := srcs[g.StudentUserID]
			if !ok || !match(src.FirstName, src.LastName) {
				continue
			}
			d, err := ToStudentL3Card(src, orgID, g.view())
			if err != nil {
				continue // not a student a company may see (04 §V.0-3)
			}
			if cards = append(cards, card{dto: d, key: *pos}); len(cards) > limit {
				break
			}
		}
		if exhausted {
			break
		}
	}
	out := CompanyStudentPage{Items: make([]StudentL3CardDTO, 0, min(len(cards), limit))}
	switch {
	case len(cards) > limit:
		cards = cards[:limit]
		out.Next = &cards[limit-1].key
	case !exhausted && pos != nil:
		out.Next = pos // the scan limit: carry on after the last inspected grant
	}
	for _, c := range cards {
		out.Items = append(out.Items, c.dto)
	}
	return out, nil
}

// cardSources returns the card facts of the students of rows a company may
// see: a live adult account and a stored profile (others are absent).
func (s *service) cardSources(ctx context.Context, rows []AccessGrant) (map[uuid.UUID]CompanySource, error) {
	out := map[uuid.UUID]CompanySource{}
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, g := range rows {
		ids = append(ids, g.StudentUserID)
	}
	users, err := s.d.Users.GetMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	profiles, err := s.d.Store.Profiles(ctx, ids)
	if err != nil {
		return nil, err
	}
	unis, depts := map[uuid.UUID]string{}, map[uuid.UUID]string{}
	for _, p := range profiles {
		u, ok := users[p.UserID]
		if !ok || u.Status != identity.StatusActive || u.IsMinor {
			continue
		}
		v := ProfileView{Profile: p, Stored: true}
		if v.UniversityName, err = cachedName(unis, p.UniversityID, s.universityName(ctx)); err != nil {
			return nil, err
		}
		if p.DepartmentID != nil {
			if v.DepartmentName, err = cachedName(depts, *p.DepartmentID, s.departmentName(ctx)); err != nil {
				return nil, err
			}
		}
		v.AvatarURL = s.avatarURL(ctx, p.UserID, p.AvatarFileID)
		out[p.UserID] = CompanySource{View: v, FirstName: u.FirstName, LastName: u.LastName, IsMinor: u.IsMinor}
	}
	return out, nil
}

// GetCompanyStudentProfile implements Service (students.profile.read_company;
// 03 S·G, 04 §V.3). Order: the company's active grant and the student's
// facts → authz (another company 404; staff 403; a company role without an
// active grant, a minor, an inactive account or no profile: 404 — the
// answer of an unknown student) → the level 3 projection → the audit
// record (the read fails if the record cannot be written).
func (s *service) GetCompanyStudentProfile(ctx context.Context, a authz.Actor, orgID, userID uuid.UUID) (StudentProfileL3DTO, error) {
	t, err := s.companyTarget(ctx, orgID, userID)
	if err != nil {
		return StudentProfileL3DTO{}, err
	}
	if err := authz.Can(ctx, a, authz.StudentsProfileReadCompany,
		companyResource(authz.TypeStudentProfile, orgID, userID, t)); err != nil {
		return StudentProfileL3DTO{}, err
	}
	if !t.visible {
		return StudentProfileL3DTO{}, ErrNotFound
	}
	src, err := s.companySource(ctx, t)
	if err != nil {
		return StudentProfileL3DTO{}, err
	}
	d, err := ToStudentProfileL3(src, orgID, t.grant.view())
	if err != nil {
		return StudentProfileL3DTO{}, err
	}
	ev := grantEvent(string(authz.StudentsProfileReadCompany), t.grant, map[string]any{
		"grant_id": t.grant.ID.String(), "contact_shared": d.Contact != nil, "clubs_shared": d.Clubs != nil,
	})
	if err := s.auditRead(ctx, a, orgID, authz.TypeStudentProfile, userID, ev); err != nil {
		return StudentProfileL3DTO{}, err
	}
	return d, nil
}

// auditRead writes the audit record of a company read in its own
// transaction (03 §6.5: every read of these actions is audited).
func (s *service) auditRead(ctx context.Context, a authz.Actor, orgID uuid.UUID, typ string, userID uuid.UUID, ev audit.Event) error {
	org, user := orgID, userID
	ev.ActorOrgID, ev.OrgID, ev.ResourceType, ev.ResourceID = &org, &org, typ, &user
	return s.d.Tx.WithTx(ctx, func(ctx context.Context) error { return s.d.Audit.Log(ctx, ev.By(a)) })
}

// companySource fills the level 3 source of a visible target: the full
// view, the names, the clubs (only with show_clubs_on_grant: the
// projection drops the sensitive ones) and the e-mail (only with
// show_contact_on_grant and the contact_info_share consent in force, read
// now: T-237).
func (s *service) companySource(ctx context.Context, t companyTarget) (CompanySource, error) {
	v, err := s.view(ctx, t.profile, true)
	if err != nil {
		return CompanySource{}, err
	}
	user := t.profile.UserID
	src := CompanySource{View: v, FirstName: t.user.FirstName, LastName: t.user.LastName, IsMinor: t.user.IsMinor}
	if s.d.Verifications != nil {
		ver, err := s.d.Verifications.ActiveMany(ctx, []uuid.UUID{user})
		if err != nil {
			return CompanySource{}, err
		}
		_, src.Verified = ver[user]
	}
	if src.SkillNames, err = s.skillNames(ctx, v.Skills); err != nil {
		return CompanySource{}, err
	}
	for _, c := range v.CVs {
		if c.CV.IsCurrent {
			at := c.CV.CreatedAt
			src.CVUploadedAt = &at
		}
	}
	if v.Privacy.ShowClubsOnGrant {
		if src.Clubs, err = s.clubFacts(ctx, user); err != nil {
			return CompanySource{}, err
		}
	}
	if v.Privacy.ShowContactOnGrant {
		if src.ContactConsent, err = s.consentActive(ctx, user, consent.PurposeContactInfoShare); err != nil {
			return CompanySource{}, err
		}
		if src.ContactConsent && s.d.Contacts != nil {
			switch c, err := s.d.Contacts.Contact(ctx, user); {
			case err == nil:
				src.Email = c.Email
			case !errors.Is(err, identity.ErrNotFound):
				return CompanySource{}, err
			}
		}
	}
	return src, nil
}

// consentActive reports whether userID's consent for purpose is in force
// (fail closed without the checker).
func (s *service) consentActive(ctx context.Context, userID uuid.UUID, purpose consent.Purpose) (bool, error) {
	if s.d.Consents == nil {
		return false, nil
	}
	ids, err := s.d.Consents.FilterActive(ctx, []uuid.UUID{userID}, purpose)
	if err != nil {
		return false, err
	}
	return slices.Contains(ids, userID), nil
}

// skillNames names the skills of a company view (none without the namer).
func (s *service) skillNames(ctx context.Context, skills []Skill) (map[uuid.UUID]string, error) {
	if s.d.SkillNames == nil || len(skills) == 0 {
		return map[uuid.UUID]string{}, nil
	}
	ids := make([]uuid.UUID, len(skills))
	for i, k := range skills {
		ids[i] = k.SkillID
	}
	return s.d.SkillNames.SkillNames(ctx, ids)
}

// clubFacts returns the student's active club memberships with their
// names; an unknown club counts as sensitive (T-240) and a sensitive club
// is marked so the projection drops it (03 Y2).
func (s *service) clubFacts(ctx context.Context, userID uuid.UUID) ([]ClubFact, error) {
	ms, err := s.d.Orgs.MembershipsOf(ctx, userID)
	if err != nil {
		return nil, err
	}
	var out []ClubFact
	var ids []uuid.UUID
	for _, m := range ms {
		if m.OrgKind != authz.OrgClub || slices.ContainsFunc(out, func(c ClubFact) bool { return c.OrgID == m.OrgID }) {
			continue
		}
		_, sensitive, err := s.clubSensitive(ctx, m.OrgID)
		if err != nil {
			return nil, err
		}
		out = append(out, ClubFact{OrgID: m.OrgID, Role: string(m.Role), Sensitive: sensitive})
		ids = append(ids, m.OrgID)
	}
	names, err := s.orgNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, c := range out {
		n, ok := names[c.OrgID]
		if !ok { // no name to show: left out
			continue
		}
		c.Name = n
		kept = append(kept, c)
	}
	return kept, nil
}

// DownloadCompanyStudentCV implements Service (students.cv.read_company;
// 03 S·G, §6.5). Order: the company's active grant and the student's facts
// → authz (as getCompanyStudentProfile) → files (503 without) → the
// current CV (none: 404) → a 1-minute signed URL of the clean file (409
// files.not_clean while the scan is pending) → the audit record of every
// call (no URL leaves without it).
func (s *service) DownloadCompanyStudentCV(ctx context.Context, a authz.Actor, orgID, userID uuid.UUID) (files.Download, error) {
	t, err := s.companyTarget(ctx, orgID, userID)
	if err != nil {
		return files.Download{}, err
	}
	if err := authz.Can(ctx, a, authz.StudentsCvReadCompany, companyResource(authz.TypeStudentCV, orgID, userID, t)); err != nil {
		return files.Download{}, err
	}
	if !t.visible {
		return files.Download{}, ErrNotFound
	}
	if s.d.Files == nil {
		return files.Download{}, errUnavailable
	}
	cvs, err := s.d.Store.CVs(ctx, userID)
	if err != nil {
		return files.Download{}, err
	}
	i := slices.IndexFunc(cvs, func(c CV) bool { return c.IsCurrent })
	if i < 0 {
		return files.Download{}, ErrNotFound
	}
	cur := cvs[i]
	dl, err := s.d.Files.PresignGet(ctx, cur.FileID, companyCVTTL)
	if err != nil {
		return files.Download{}, err
	}
	ev := grantEvent(string(authz.StudentsCvReadCompany), t.grant, map[string]any{
		"grant_id": t.grant.ID.String(), "cv_id": cur.ID.String(), "file_id": cur.FileID.String(),
	})
	if err := s.auditRead(ctx, a, orgID, authz.TypeStudentCV, userID, ev); err != nil {
		return files.Download{}, err
	}
	return dl, nil
}

// cachedName returns the name of id from cache, or reads it with get.
func cachedName(cache map[uuid.UUID]string, id uuid.UUID, get func(uuid.UUID) (string, error)) (string, error) {
	if n, ok := cache[id]; ok {
		return n, nil
	}
	n, err := get(id)
	if err != nil {
		return "", err
	}
	cache[id] = n
	return n, nil
}

// universityName reads a university's name ("" when the catalog no longer
// has it).
func (s *service) universityName(ctx context.Context) func(uuid.UUID) (string, error) {
	return func(id uuid.UUID) (string, error) {
		u, err := s.d.Catalog.GetUniversity(ctx, id)
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return "", nil
		case err != nil:
			return "", err
		}
		return u.Name, nil
	}
}

// departmentName reads a department's name ("" when the catalog no longer
// has it).
func (s *service) departmentName(ctx context.Context) func(uuid.UUID) (string, error) {
	return func(id uuid.UUID) (string, error) {
		d, err := s.d.Catalog.GetDepartment(ctx, id)
		switch {
		case errors.Is(err, directory.ErrNotFound):
			return "", nil
		case err != nil:
			return "", err
		}
		return d.Name, nil
	}
}
