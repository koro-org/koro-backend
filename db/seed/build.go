package seed

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/outreach"
	"koro/internal/platform/testutil/fake"
	"koro/internal/platform/textx"
)

// kinds are the ID spaces of the plan, in insertion order; an ID's time
// part is the kind's base plus the row's ordinal (demoID).
var kinds = []string{"user", "role", "edu", "org", "member", "ent", "usage", "segment", "event", "form", "formver",
	"reg", "ticket", "checkin", "post", "notice"}

// istanbul is the zone of the demo's local times (events at 18:00).
var istanbul = func() *time.Location {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		return time.FixedZone("TRT", 3*60*60) // no DST since 2016
	}
	return loc
}()

type student struct {
	id, uniOrg     uuid.UUID
	uni            int
	first, last    string
	minor          bool
	dept           *Department
	year           int
	clubs          []*club // active memberships (admin included)
	adminOf        *club
	followsCompany *company
}

type club struct {
	orgID     uuid.UUID
	uni       int
	spec      clubSpec
	sensitive bool
	reason    string
	admin     *student
	members   []*student
}

type company struct {
	orgID   uuid.UUID
	spec    companySpec
	owner   uuid.UUID
	ent     uuid.UUID // targeted_post grant (active companies)
	used    int
	segment uuid.UUID
}

type event struct {
	id, orgID, creator uuid.UUID
	orgKind            string
	uni                int
	club               *club
	title, slug        string
	category           string
	status             string
	starts, ends       time.Time
	published          *time.Time
	capacity           *int
	waitlist, checkin  bool
	requiresReg        bool
	manual             bool
	visibility         string
	sensitive          bool
	formVersion        *uuid.UUID
}

type builder struct {
	p   *Plan
	o   Options
	ref Reference
	now time.Time
	day time.Time // today 00:00 in Istanbul
	rng *rand.Rand
	f   *fake.Faker
	pw  any // nil or the password hash

	admin1, admin2 uuid.UUID
	reps           []uuid.UUID // per university
	students       []*student
	byUni          [][]*student
	clubs          []*club
	companies      []*company
	events         []*event
	ord            map[string]int
	schema         events.FormSchema
}

func newBuilder(ref Reference, o Options) *builder {
	b := &builder{
		p: &Plan{}, o: o, ref: ref, now: o.Now.UTC().Truncate(time.Second), f: fake.New(o.Seed),
		rng: rand.New(rand.NewPCG(o.Seed, o.Seed^0x5eed)), //nolint:gosec // G404: synthetic demo data, not security relevant
		ord: map[string]int{},
	}
	l := b.now.In(istanbul)
	b.day = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, istanbul)
	if o.PasswordHash != "" {
		b.pw = o.PasswordHash
	}
	return b
}

// id is the deterministic ID of the n-th row of kind.
func (b *builder) id(kind string, n int) uuid.UUID {
	base := slices.Index(kinds, kind)
	if base < 0 {
		panic("seed: unknown id kind " + kind)
	}
	return demoID(kind+":"+strconv.Itoa(n), base*1_000_000+n)
}

// next returns the next ordinal of kind (rows without a natural index).
func (b *builder) next(kind string) int {
	b.ord[kind]++
	return b.ord[kind]
}

func (b *builder) intn(n int) int { return b.rng.IntN(n) }

// at is the day offset d (days from today) at hh:mm Istanbul time, in UTC.
func (b *builder) at(d, hh, mm int) time.Time {
	return b.day.AddDate(0, 0, d).Add(time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute).UTC()
}

func (b *builder) build() error {
	if err := b.formSchema(); err != nil {
		return err
	}
	b.platformStaff()
	b.studentsAndReps()
	b.clubOrgs()
	b.companyOrgs()
	b.memberships()
	b.studentRows()
	b.feedSettings()
	b.outreach()
	if err := b.eventRows(); err != nil {
		return err
	}
	b.postRows()
	b.notice()
	u := b.ref.Universities[0]
	b.p.Demo.MainUniversity = u
	return nil
}

// user adds an active, verified account with the demo password (if any).
func (b *builder) user(n int, email, first, last, persona string, birth *time.Time) uuid.UUID {
	return b.account(n, email, first, last, persona, birth, b.pw)
}

// account adds an active, verified account with password hash pw (nil:
// none).
func (b *builder) account(n int, email, first, last, persona string, birth *time.Time, pw any) uuid.UUID {
	id := b.id("user", n)
	created := b.now.AddDate(0, 0, -90)
	var bd any
	if birth != nil {
		bd = date(*birth)
	}
	b.p.add("users", "id", id, "status", "active", "email", email, "email_normalized", textx.NormalizeEmail(email),
		"email_verified_at", created, "password_hash", pw, "first_name", first, "last_name", last, "birth_date", bd,
		"signup_persona", persona, "locale", "tr-TR", "created_at", created, "updated_at", created)
	return id
}

// role adds an active platform role assignment.
func (b *builder) role(userID uuid.UUID, role, reason string, requestedBy, approvedBy *uuid.UUID) {
	at := b.now.AddDate(0, 0, -89)
	var approvedAt any
	if approvedBy != nil {
		approvedAt = at
	}
	b.p.add("platform_role_assignments", "id", b.id("role", b.next("role")), "user_id", userID, "role", role,
		"reason", reason, "status", "active", "requested_by", requestedBy, "requested_at", at, "approved_by", approvedBy,
		"approved_at", approvedAt, "granted_at", at)
}

// platformStaff opens the platform roles: two bootstrap platform admins
// (four eyes never deadlocks, 06 H-17), two finance admins approved by the
// second admin, a DPO, a moderator and a support agent. A privileged
// account never gets the demo password (06 H-66, T-287): it is opened by a
// password reset and TOTP enrolment, so a shared demo password never opens
// an account without MFA that can read personal data or move money.
func (b *builder) platformStaff() {
	staff := []struct{ email, role string }{
		{AdminEmail1, "platform_admin"}, {AdminEmail2, "platform_admin"}, {FinanceEmail1, "finance_admin"},
		{FinanceEmail2, "finance_admin"}, {DPOEmail, "dpo"}, {ModeratorEmail, "moderator"}, {SupportEmail, "support"},
	}
	ids := make([]uuid.UUID, len(staff))
	for i, s := range staff {
		ids[i] = b.account(i+1, s.email, b.f.FirstName(), b.f.LastName(), "platform", nil, nil)
		b.p.Accounts = append(b.p.Accounts, Account{Email: s.email, Role: s.role, NoPassword: true})
	}
	b.admin1, b.admin2 = ids[0], ids[1]
	b.role(b.admin1, "platform_admin", "bootstrap", nil, nil)
	b.role(b.admin2, "platform_admin", "bootstrap", nil, nil)
	a1, a2 := b.admin1, b.admin2
	for i, s := range staff[2:] {
		reason := "Demo ortamı: " + s.role + " rolü"
		if s.role == "finance_admin" {
			b.role(ids[i+2], s.role, reason, &a1, &a2) // privileged: a second platform_admin approved it
			continue
		}
		b.role(ids[i+2], s.role, reason, &a1, nil)
	}
}

// studentsAndReps creates the accounts of the students and of one
// university_rep per university (memberships come later).
func (b *builder) studentsAndReps() {
	unis := b.ref.Universities
	b.byUni = make([][]*student, len(unis))
	for i := range StudentCount {
		uni := i % minUniversities
		s := &student{uni: uni, uniOrg: unis[uni].OrgID, first: b.f.FirstName(), last: b.f.LastName(),
			minor: i%minorEvery == minorEvery-1}
		email := fmt.Sprintf("%s.%s.%03d@%s", fake.ASCIIFold(s.first), fake.ASCIIFold(s.last), i, fake.EmailDomain)
		switch i {
		case 0:
			email = StudentEmail
		case 3:
			email = ClubAdminEmail
		case minorEvery - 1:
			email = MinorEmail // university 0 (24 % 3 == 0)
		}
		var birth time.Time
		if s.minor {
			birth = b.day.AddDate(-17, 0, -(30 + b.intn(300))) // 17 years old
		} else {
			birth = b.day.AddDate(-(18 + b.intn(8)), 0, -(1 + b.intn(360)))
		}
		if deps := unis[uni].Departments; len(deps) > 0 {
			d := deps[b.intn(len(deps))]
			s.dept = &d
		}
		years := 4
		if s.dept != nil && s.dept.DurationYears > 0 {
			years = min(s.dept.DurationYears, 6)
		}
		s.year = 1 + b.intn(years)
		s.id = b.user(100+i, email, s.first, s.last, "student", &birth)
		b.students = append(b.students, s)
		b.byUni[uni] = append(b.byUni[uni], s)
	}
	b.p.Accounts = append(b.p.Accounts, Account{Email: StudentEmail, Role: "student"},
		Account{Email: ClubAdminEmail, Role: "student, club_admin"}, Account{Email: MinorEmail, Role: "student under 18"})
	for k := range unis {
		email := RepEmail
		if k > 0 {
			email = fmt.Sprintf("demo.temsilci%d@%s", k+1, fake.EmailDomain)
		}
		b.reps = append(b.reps, b.user(1000+k, email, b.f.FirstName(), b.f.LastName(), "university", nil))
		b.p.Accounts = append(b.p.Accounts, Account{Email: email, Role: "university_rep (" + unis[k].Name + ")"})
	}
}

// org adds an organizations row.
func (b *builder) org(n int, kind, name, slug, status, city string, verified bool, website string) uuid.UUID {
	id := b.id("org", n)
	created := b.now.AddDate(0, 0, -80)
	var vAt, vBy, web any
	if verified {
		vAt, vBy = created, b.admin1
	}
	if website != "" {
		web = website
	}
	b.p.add("organizations", "id", id, "kind", kind, "name", name, "slug", slug, "status", status,
		"description", name+": Koro demo ortamının sentetik verisidir; gerçek bir kuruluşu temsil etmez.",
		"website_url", web, "city", city, "verified_at", vAt, "verified_by", vBy, "created_by", b.admin1,
		"created_at", created, "updated_at", created)
	return id
}

// uniSlug is the slug part of a university ("ODTÜ" → "odtu").
func uniSlug(u University, k int) string {
	var sb strings.Builder
	for _, r := range fake.ASCIIFold(cmp.Or(u.ShortName, u.Name)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			sb.WriteRune(r)
		}
	}
	return cmp.Or(sb.String(), "uni"+strconv.Itoa(k+1))
}

// clubOrgs creates eight clubs per university; the last one of the first
// two universities is sensitive. Every club but one gets a club_admin (an
// adult student of its university).
func (b *builder) clubOrgs() {
	for k, u := range b.ref.Universities {
		adults := slices.DeleteFunc(slices.Clone(b.byUni[k]), func(s *student) bool { return s.minor })
		next := 0
		for j := range clubsPerUni {
			c := &club{uni: k, spec: clubCatalog[(k*4+j)%len(clubCatalog)]}
			if j == clubsPerUni-1 && k < len(sensitiveClubs) {
				c.spec, c.sensitive, c.reason = sensitiveClubs[k].clubSpec, true, sensitiveClubs[k].Reason
			}
			c.orgID = b.org(k*100+j, "club", c.spec.Name, "demo-"+uniSlug(u, k)+"-"+c.spec.Slug, "active", u.City, false, "")
			created := b.now.AddDate(0, 0, -80)
			var setBy, setAt, reason any
			if c.sensitive {
				setBy, setAt, reason = b.admin1, created, c.reason
			}
			switch {
			case k == 0 && j == 0:
				c.admin = b.students[3] // ClubAdminEmail
			case k == 2 && j == 6:
				// unclaimed: no admin yet (a claim can be demoed)
			default:
				for adults[next] == b.students[0] || adults[next] == b.students[3] {
					next++
				}
				c.admin, next = adults[next], next+1
			}
			claim := "unclaimed"
			if c.admin != nil {
				claim = "claimed"
				c.admin.adminOf = c
			}
			b.p.add("clubs", "org_id", c.orgID, "org_kind", "club", "university_id", u.ID, "category", c.spec.Category,
				"is_sensitive", c.sensitive, "sensitive_reason", reason, "sensitivity_set_by", setBy, "sensitivity_set_at", setAt,
				"join_policy", c.spec.JoinPolicy, "claim_status", claim, "founded_year", 1995+b.intn(28),
				"created_at", created, "updated_at", created)
			b.clubs = append(b.clubs, c)
			if c.sensitive {
				b.p.Demo.SensitiveClubOrgIDs = append(b.p.Demo.SensitiveClubOrgIDs, c.orgID)
			}
		}
	}
	b.p.Demo.MainClubOrgID = b.clubs[0].orgID
}

// companyOrgs creates the companies: three active (verified) ones with a
// targeted_post grant and a pending_review one.
func (b *builder) companyOrgs() {
	for k, spec := range companyCatalog {
		c := &company{spec: spec}
		status := "active"
		if spec.Pending {
			status = "pending_review"
		}
		c.orgID = b.org(1000+k, "company", spec.Name, "demo-"+spec.Slug, status, spec.City, !spec.Pending,
			"https://"+spec.Slug+"."+fake.EmailDomain)
		created := b.now.AddDate(0, 0, -80)
		b.p.add("companies", "org_id", c.orgID, "org_kind", "company", "sector_code", spec.Sector, "size_band", spec.Size,
			"hq_city", spec.City, "created_at", created, "updated_at", created)
		email := CompanyEmail
		if k > 0 {
			email = fmt.Sprintf("demo.sirket%d@%s", k+1, fake.EmailDomain)
		}
		c.owner = b.user(2000+k, email, b.f.FirstName(), b.f.LastName(), "company", nil)
		b.member(c.orgID, "company", c.owner, "company_owner", "admin", nil)
		b.p.Accounts = append(b.p.Accounts, Account{Email: email, Role: "company_owner (" + spec.Name + ", " + status + ")"})
		if k == 0 {
			hr := b.user(2100, "demo.ik@"+fake.EmailDomain, b.f.FirstName(), b.f.LastName(), "company", nil)
			b.member(c.orgID, "company", hr, "recruiter", "invitation", &c.owner)
			b.p.Accounts = append(b.p.Accounts, Account{Email: "demo.ik@" + fake.EmailDomain, Role: "recruiter (" + spec.Name + ")"})
		}
		b.companies = append(b.companies, c)
		if spec.Pending {
			b.p.Demo.PendingCompanyOrgID = c.orgID
		}
	}
	b.p.Demo.CompanyOrgID = b.companies[0].orgID
}

// member adds an active membership.
func (b *builder) member(orgID uuid.UUID, kind string, userID uuid.UUID, role, source string, approvedBy *uuid.UUID) {
	joined := b.now.AddDate(0, 0, -(30 + b.intn(40)))
	var requested any
	if source == "join_request" {
		requested = joined.Add(-time.Duration(1+b.intn(48)) * time.Hour)
	}
	b.p.add("org_members", "id", b.id("member", b.next("member")), "org_id", orgID, "org_kind", kind, "user_id", userID,
		"role", role, "status", "active", "source", source, "requested_at", requested, "joined_at", joined,
		"approved_by", approvedBy, "created_at", joined, "updated_at", joined)
}

// memberships: club admins, university reps, and every student in one to
// three clubs of their university (StudentEmail always in the main club).
func (b *builder) memberships() {
	for _, c := range b.clubs {
		if c.admin != nil {
			b.member(c.orgID, "club", c.admin.id, "club_admin", "claim", nil)
			c.admin.clubs = append(c.admin.clubs, c)
			c.members = append(c.members, c.admin)
		}
	}
	for k, rep := range b.reps {
		a1 := b.admin1
		b.member(b.ref.Universities[k].OrgID, "university", rep, "university_rep", "admin", &a1)
	}
	for i, s := range b.students {
		own := b.clubs[s.uni*clubsPerUni : (s.uni+1)*clubsPerUni]
		want := 1 + b.intn(3)
		picks := b.rng.Perm(len(own))
		if i == 0 {
			picks = append([]int{0}, slices.DeleteFunc(picks, func(p int) bool { return p == 0 })...)
		}
		for _, p := range picks {
			c := own[p]
			if len(s.clubs) >= want+boolInt(s.adminOf != nil) {
				break
			}
			if c == s.adminOf {
				continue
			}
			var approver *uuid.UUID
			if c.spec.JoinPolicy == "approval" && c.admin != nil {
				approver = &c.admin.id
			}
			b.member(c.orgID, "club", s.id, "member", "join_request", approver)
			s.clubs = append(s.clubs, c)
			c.members = append(c.members, s)
		}
		if !s.minor && b.intn(3) == 0 {
			s.followsCompany = b.companies[b.intn(len(b.companies)-1)] // an active company
		}
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// audienceKeys is the students module's rule (02 §9.1): all, uni, dept,
// year and the NON-sensitive clubs, sorted.
func (s *student) audienceKeys(u University) []string {
	keys := []string{"all", "uni:" + u.ID.String(), "year:" + strconv.Itoa(s.year)}
	if s.dept != nil {
		keys = append(keys, "dept:"+s.dept.ID.String())
	}
	for _, c := range s.clubs {
		if !c.sensitive {
			keys = append(keys, "club:"+c.orgID.String())
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// studentRows: each student's document verification (decided by the
// first admin, valid for a year), profile with audience columns, and the
// follows (memberships and a company).
func (b *builder) studentRows() {
	verified := b.now.AddDate(0, 0, -60)
	for i, s := range b.students {
		u := b.ref.Universities[s.uni]
		b.p.add("edu_verifications", "id", b.id("edu", i), "user_id", s.id, "method", "document", "university_id", u.ID,
			"status", "verified", "verified_at", verified, "expires_at", verified.AddDate(1, 0, 0), "reviewed_by", b.admin1,
			"review_note", "Demo: sentetik öğrenci belgesi", "created_at", verified, "updated_at", verified)
		var dept, degree any = nil, "bachelor"
		years := 4
		if s.dept != nil {
			dept, degree = s.dept.ID, cmp.Or(s.dept.DegreeLevel, "bachelor")
			if s.dept.DurationYears > 0 {
				years = s.dept.DurationYears
			}
		}
		grad := time.Date(b.now.Year()+max(years-s.year, 0)+1, time.June, 1, 0, 0, 0, 0, time.UTC)
		b.p.add("student_profiles", "user_id", s.id, "university_id", u.ID, "department_id", dept, "degree_level", degree,
			"class_year", s.year, "expected_graduation", date(grad), "status", "active", "city", u.City,
			"completed_sections", []string{"basics"}, "audience_keys", s.audienceKeys(u), "audience_eligible", !s.minor,
			"audience_refreshed_at", b.now, "created_at", verified, "updated_at", verified)
		for _, c := range s.clubs {
			b.p.add("org_follows", "user_id", s.id, "org_id", c.orgID, "source", "auto_membership", "created_at", verified)
		}
		if s.followsCompany != nil {
			b.p.add("org_follows", "user_id", s.id, "org_id", s.followsCompany.orgID, "source", "manual", "created_at", verified)
		}
	}
}

// feedSettings: every demo author is trusted (its posts publish without
// the first-posts review, R-003 §1.5.6).
func (b *builder) feedSettings() {
	orgs := []uuid.UUID{b.ref.PlatformOrgID}
	for _, u := range b.ref.Universities {
		orgs = append(orgs, u.OrgID)
	}
	for _, c := range b.clubs {
		orgs = append(orgs, c.orgID)
	}
	for _, c := range b.companies {
		if !c.spec.Pending {
			orgs = append(orgs, c.orgID)
		}
	}
	at := b.now.AddDate(0, 0, -80)
	for _, o := range orgs {
		b.p.add("feed_org_settings", "org_id", o, "trust_level", "trusted", "approved_posts_count", 10,
			"updated_by", b.admin1, "created_at", at, "updated_at", at)
	}
}

// eligibleIn counts the eligible students of university k.
func (b *builder) eligibleIn(k int) int {
	n := 0
	for _, s := range b.byUni[k] {
		if !s.minor {
			n++
		}
	}
	return n
}

// outreach: every active company gets five targeted posts (an admin grant,
// 06 H-40) and a segment of one university (the estimate rounded as
// outreach.Bucket; the first company's segment is the main university).
func (b *builder) outreach() {
	from, to := b.now.AddDate(0, 0, -7), b.now.AddDate(0, 6, 0)
	for k, c := range b.companies {
		if c.spec.Pending {
			continue
		}
		c.ent = b.id("ent", k)
		used := 0
		if k == 0 {
			used = 1 // the seeded targeted post below
		}
		c.used = used
		b.p.add("entitlements", "id", c.ent, "org_id", c.orgID, "feature", "targeted_post", "kind", "quota",
			"quantity_total", 5, "quantity_used", used, "valid_from", from, "valid_to", to, "status", "active",
			"source_type", "admin_grant", "granted_by", b.admin1, "activated_at", from, "created_at", from, "updated_at", from)
		u := b.ref.Universities[k%len(b.ref.Universities)]
		c.segment = b.id("segment", k)
		var count any
		if n := b.eligibleIn(k % len(b.ref.Universities)); n >= outreach.KThreshold {
			count = outreach.Bucket(n)
		}
		b.p.add("segments", "id", c.segment, "org_id", c.orgID, "name", u.Name+" öğrencileri",
			"definition", jsonb(map[string]any{"uni": []string{u.ID.String()}}), "audience_keys", []string{"uni:" + u.ID.String()},
			"audience_dim_count", 1, "last_count", count, "last_counted_at", b.now, "created_by", c.owner,
			"created_at", from, "updated_at", from)
	}
	b.p.Demo.EntitlementID, b.p.Demo.SegmentID = b.companies[0].ent, b.companies[0].segment
}

// formSchema parses the demo registration form (two innocuous fields: no
// lint hit, so its versions need no override).
func (b *builder) formSchema() error {
	raw := `{"schema_version":1,"fields":[` +
		`{"key":"sinif","type":"single_select","label":{"tr":"Kaçıncı sınıftasın?","en":"Which year are you in?"},"required":true,` +
		`"options":[{"value":"1","label":{"tr":"1. sınıf"}},{"value":"2","label":{"tr":"2. sınıf"}},{"value":"3","label":{"tr":"3. sınıf"}},` +
		`{"value":"4","label":{"tr":"4. sınıf"}},{"value":"5","label":{"tr":"5. sınıf ve üzeri"}}]},` +
		`{"key":"beklenti","type":"long_text","label":{"tr":"Etkinlikten beklentin nedir?"},"required":false,"validation":{"max_length":500}}]}`
	s, err := events.ParseFormSchema([]byte(raw), "seed")
	if err != nil {
		return fmt.Errorf("seed: demo form: %w", err)
	}
	b.schema = s
	return nil
}

// manualCode is a ticket's manual code: 8 Crockford base32 characters
// (02 §11.7), unique per event.
func (b *builder) manualCode(used map[string]bool) string {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for {
		var sb strings.Builder
		for range 8 {
			sb.WriteByte(alphabet[b.intn(len(alphabet))])
		}
		if c := sb.String(); !used[c] {
			used[c] = true
			return c
		}
	}
}

// postBody is a one-paragraph Tiptap document and its plain text.
func postBody(text string) (typed, string) {
	return jsonb(map[string]any{"type": "doc", "content": []any{
		map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text}}},
	}}), text
}

// noticeBody is the demo privacy notice.
const noticeBody = "# Aydınlatma Metni (demo)\n\nBu metin Koro demo ortamı içindir ve hukuki metnin yerini tutmaz. " +
	"Demo ortamındaki tüm kişiler, kulüpler ve şirketler sentetiktir."

// notice publishes a demo privacy notice for every audience when none is
// current (the current-notice unique index makes it a no-op otherwise):
// registration needs one (04 §1.12).
func (b *builder) notice() {
	sum := sha256.Sum256([]byte(noticeBody))
	at := b.now.AddDate(0, 0, -90)
	b.p.add("notice_versions", "id", b.id("notice", 0), "kind", "privacy_notice", "audience", "all", "locale", "tr-TR",
		"version", "2026.10.01", "title", "Aydınlatma Metni (demo)", "body_md", noticeBody, "body_sha256", sum[:],
		"published_at", at, "created_by", b.admin1, "created_at", at)
}
