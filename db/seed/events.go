package seed

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/outreach"
)

// Event shapes of the clubs (by club index modulo 6; a sensitive club's
// event is always members-only, R-003 §2.2).
const (
	shapePast       = iota // ended: completed, check-ins and no-shows, attendance closed
	shapeOpen              // upcoming, capacity 40, auto confirmation, check-in
	shapeFull              // upcoming, capacity 12, full, waiting list
	shapeManual            // upcoming, application (manual mode)
	shapeUniversity        // upcoming, university-only, no capacity
	shapeNoReg             // upcoming, members-only, no registration
	shapeCount
)

// eventRows creates the events (35 with three universities), their forms
// and registrations, tickets and check-ins.
func (b *builder) eventRows() error {
	n := 0
	for ci, c := range b.clubs {
		if c.admin == nil {
			continue // an unclaimed club has nobody to organise
		}
		shape := ci % shapeCount
		e := b.clubEvent(n, c, shape)
		n++
		if err := b.eventWithRegs(e, shape); err != nil {
			return err
		}
		if ci%clubsPerUni < 2 && !c.sensitive { // a second, public talk
			e2 := b.clubEvent(n, c, shapeOpen)
			e2.capacity, e2.title = nil, "Söyleşi: "+e2.title
			e2.slug += "-soylesi"
			n++
			if err := b.eventWithRegs(e2, -1); err != nil {
				return err
			}
		}
	}
	main := b.clubs[0]
	draft := b.clubEvent(n, main, shapeOpen)
	draft.status, draft.published, draft.title, draft.slug = "draft", nil, "Taslak: Dönem Sonu Buluşması", "taslak-donem-sonu"
	n++
	if err := b.eventWithRegs(draft, -1); err != nil {
		return err
	}
	cancelled := b.clubEvent(n, b.clubs[clubsPerUni+1], shapeOpen)
	cancelled.status, cancelled.title, cancelled.slug = "cancelled", "İptal Edildi: Kampüs Gezisi", "iptal-kampus-gezisi"
	n++
	if err := b.eventWithRegs(cancelled, -1); err != nil {
		return err
	}
	for k, u := range b.ref.Universities {
		e := &event{
			id: b.id("event", n), orgID: u.OrgID, orgKind: "university", creator: b.reps[k], uni: k,
			title: eventTitles["university"][k%len(eventTitles["university"])], category: "career",
			status: "published", starts: b.at(10+7*k, 10, 0), visibility: "public", requiresReg: true, checkin: true,
		}
		e.ends, e.slug = e.starts.Add(6*time.Hour), "demo-"+uniSlug(u, k)+"-"+strconv.Itoa(n)
		e.capacity = new(200)
		e.published = new(b.now.AddDate(0, 0, -10))
		n++
		if err := b.eventWithRegs(e, shapeOpen); err != nil {
			return err
		}
	}
	return nil
}

// clubEvent is the event of club c in the given shape.
func (b *builder) clubEvent(n int, c *club, shape int) *event {
	titles := eventTitles[c.spec.Category]
	e := &event{
		id: b.id("event", n), orgID: c.orgID, orgKind: "club", creator: c.admin.id, uni: c.uni, club: c,
		title: titles[n%len(titles)], slug: "etkinlik-" + strconv.Itoa(n), category: eventCategory(c.spec.Category),
		status: "published", visibility: "public", requiresReg: true, checkin: true,
	}
	day := 3 + (n*5)%35
	switch {
	case c.sensitive:
		e.visibility, e.sensitive, e.checkin = "members", true, false
	case shape == shapePast:
		day = -(5 + n%30)
		e.status = "completed"
	case shape == shapeFull:
		e.capacity, e.waitlist = new(12), true
	case shape == shapeOpen:
		e.capacity = new(40)
	case shape == shapeManual:
		e.manual, e.capacity = true, new(25)
	case shape == shapeUniversity:
		e.visibility = "university"
	case shape == shapeNoReg:
		e.visibility, e.requiresReg, e.checkin = "members", false, false
	}
	e.starts = b.at(day, 18, 0)
	e.ends = e.starts.Add(2 * time.Hour)
	e.published = new(earlier(e.starts, b.now).AddDate(0, 0, -14))
	return e
}

// eventCategory maps a club category to an event category.
func eventCategory(club string) string {
	switch club {
	case "technology", "academic":
		return "workshop"
	case "hobby", "sports":
		return "sports"
	case "arts":
		return "culture"
	case "career", "entrepreneurship":
		return "career"
	case "social_responsibility":
		return "volunteering"
	}
	return "talk"
}

// eventWithRegs writes the event, its form and its registrations.
func (b *builder) eventWithRegs(e *event, shape int) error {
	u := b.ref.Universities[e.uni]
	created := b.now.AddDate(0, 0, -20)
	if e.published != nil && e.published.Before(created) {
		created = e.published.Add(-time.Hour)
	}
	var visUni, cancelledAt, cancelReason, finalizedAt any
	if e.visibility == "university" {
		visUni = u.ID
	}
	if e.status == "cancelled" {
		cancelledAt, cancelReason = b.now.AddDate(0, 0, -2), "Hava koşulları nedeniyle iptal edildi."
	}
	if e.status == "completed" && e.checkin {
		finalizedAt = e.ends.Add(24 * time.Hour)
	}
	formID := b.id("form", b.next("form"))
	var draft any = rawJSON([]byte(`{"schema_version":1,"fields":[]}`))
	if e.requiresReg {
		draft = rawJSON(b.schema.Canonical())
	}
	regs, err := b.registrations(e, shape)
	if err != nil {
		return err
	}
	confirmed := 0
	for _, r := range regs {
		if r.status == "confirmed" || r.status == "checked_in" {
			confirmed++
		}
	}
	b.p.add("events", "id", e.id, "org_id", e.orgID, "created_by", e.creator, "title", e.title, "slug", e.slug,
		"summary", e.title+" — "+orgName(b, e)+" etkinliği (demo).",
		"description_text", "Bu etkinlik Koro demo ortamının sentetik verisidir.", "category", e.category,
		"format", "in_person", "location_name", venues[b.intn(len(venues))], "city", u.City,
		"starts_at", e.starts, "ends_at", e.ends, "visibility", e.visibility, "visibility_university_id", visUni,
		"status", e.status, "published_at", e.published, "cancelled_at", cancelledAt, "cancel_reason", cancelReason,
		"requires_registration", e.requiresReg, "registration_mode", map[bool]string{true: "manual", false: "auto"}[e.manual],
		"capacity", e.capacity, "confirmed_count", confirmed, "waitlist_enabled", e.waitlist, "checkin_enabled", e.checkin,
		"attendance_finalized_at", finalizedAt, "is_sensitive_category", e.sensitive, "created_at", created, "updated_at", created)
	b.p.add("event_forms", "id", formID, "event_id", e.id, "org_id", e.orgID, "draft_schema", draft,
		"updated_by", e.creator, "created_at", created, "updated_at", created)
	if e.requiresReg && e.status != "draft" {
		v := b.id("formver", b.next("formver"))
		e.formVersion = &v
		b.p.add("event_form_versions", "id", v, "form_id", formID, "event_id", e.id, "version_no", 1,
			"schema", rawJSON(b.schema.Canonical()), "schema_sha256", b.schema.SHA256(), "field_count", len(b.schema.Fields),
			"has_sensitive_fields", false, "lint_report", rawJSON([]byte(`{"blocked":[],"warn_special":[],"review_identifier":[]}`)),
			"published_by", e.creator, "published_at", created)
		b.p.After = append(b.p.After, Stmt{
			SQL:  `UPDATE event_forms SET current_version_id = $2 WHERE id = $1 AND current_version_id IS NULL`,
			Args: []any{formID, v},
		})
	}
	return b.writeRegistrations(e, regs)
}

func orgName(b *builder, e *event) string {
	if e.club != nil {
		return e.club.spec.Name
	}
	return b.ref.Universities[e.uni].Name
}

// registration is a planned registration.
type registration struct {
	s      *student
	status string
}

// registrations picks the registrants of e by shape: students of the
// organiser's university (members of the club for a members-only event),
// each at most once.
func (b *builder) registrations(e *event, shape int) ([]registration, error) {
	if !e.requiresReg || e.status == "draft" || e.status == "cancelled" {
		return nil, nil
	}
	var pool []*student
	if e.visibility == "members" && e.club != nil {
		pool = slices.Clone(e.club.members)
	} else {
		pool = slices.Clone(b.byUni[e.uni])
	}
	pool = slices.DeleteFunc(pool, func(s *student) bool { return s.id == e.creator })
	b.rng.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	var plan []string
	rep := func(status string, n int) {
		for range n {
			plan = append(plan, status)
		}
	}
	switch {
	case e.sensitive:
		rep("confirmed", 5)
	case e.status == "completed":
		rep("checked_in", 11)
		rep("no_show", 3)
		rep("cancelled", 1)
	case shape == shapeFull:
		rep("confirmed", *e.capacity)
		rep("waitlisted", 3)
	case shape == shapeManual:
		rep("confirmed", 4)
		rep("pending", 6)
	case shape == shapeUniversity:
		rep("confirmed", 10)
	case e.orgKind == "university":
		rep("confirmed", 25)
	case shape == -1:
		rep("confirmed", 8)
	default:
		rep("confirmed", 20)
	}
	if len(plan) > len(pool) {
		plan = plan[:len(pool)]
	}
	if e.capacity != nil && count(plan, "confirmed")+count(plan, "checked_in") > *e.capacity {
		return nil, fmt.Errorf("seed: event %s over capacity", e.slug)
	}
	out := make([]registration, len(plan))
	for i, st := range plan {
		out[i] = registration{s: pool[i], status: st}
	}
	return out, nil
}

func count(xs []string, x string) int {
	n := 0
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return n
}

// writeRegistrations writes the registrations of e with their tickets
// (token derived with CRYPTO_TOKEN_KEYS, so a seeded QR checks in) and the
// check-ins of the attended ones.
func (b *builder) writeRegistrations(e *event, regs []registration) error {
	codes := map[string]bool{}
	keyID := b.o.Tickets.ActiveKeyID()
	for i, r := range regs {
		id := b.id("reg", b.next("reg"))
		created := e.published.Add(time.Duration(1+b.intn(72)) * time.Hour)
		if created.After(b.now) {
			created = b.now.Add(-time.Hour)
		}
		var confirmedAt, waitlistedAt, cancelledAt, cancelledBy, checkedIn, approvedBy, approvedAt any
		switch r.status {
		case "confirmed", "checked_in", "no_show":
			confirmedAt = created
			if e.manual {
				approvedBy, approvedAt = e.creator, created.Add(time.Hour)
				confirmedAt = approvedAt
			}
		case "waitlisted":
			waitlistedAt = created
		case "cancelled":
			cancelledAt, cancelledBy = created.Add(time.Hour), "user"
		}
		scan := e.starts.Add(time.Duration(b.intn(25)) * time.Minute)
		if r.status == "checked_in" {
			checkedIn = scan
		}
		answers := map[string]any{"sinif": strconv.Itoa(min(r.s.year, 5))}
		if i%2 == 0 {
			answers["beklenti"] = expectations[b.intn(len(expectations))]
		}
		b.p.add("event_registrations", "id", id, "event_id", e.id, "org_id", e.orgID, "user_id", r.s.id, "status", r.status,
			"form_version_id", e.formVersion, "answers", jsonb(answers), "source", "web", "waitlisted_at", waitlistedAt,
			"confirmed_at", confirmedAt, "approved_by", approvedBy, "approved_at", approvedAt, "cancelled_at", cancelledAt,
			"cancelled_by_kind", cancelledBy, "checked_in_at", checkedIn, "answers_purge_at", e.ends.AddDate(0, 0, 90),
			"created_at", created, "updated_at", created)
		if r.status != "confirmed" && r.status != "checked_in" && r.status != "no_show" {
			continue
		}
		token, err := events.TicketToken(b.o.Tickets, keyID, id, 1)
		if err != nil {
			return fmt.Errorf("seed: ticket token: %w", err)
		}
		ticket := b.id("ticket", b.next("ticket"))
		b.p.add("registration_tickets", "id", ticket, "registration_id", id, "event_id", e.id, "version", 1,
			"key_id", keyID, "token_lookup", events.TicketLookup(token), "manual_code", b.manualCode(codes),
			"status", "active", "issued_at", confirmedAt)
		if r.status == "checked_in" {
			n := b.next("checkin")
			b.p.add("event_checkins", "id", b.id("checkin", n), "event_id", e.id, "org_id", e.orgID, "registration_id", id,
				"ticket_id", ticket, "staff_user_id", e.creator, "method", "qr", "device_id", "demo-kapi-1",
				"client_scan_id", demoID("scan:"+strconv.Itoa(n), n), "scanned_at", scan, "received_at", scan)
		}
	}
	b.events = append(b.events, e)
	return nil
}

// postRows creates the feed: club, university, company and platform
// posts (published over the last two months), the event shares of the
// upcoming events, one targeted post of the main company and the likes.
func (b *builder) postRows() {
	adults := slices.DeleteFunc(slices.Clone(b.students), func(s *student) bool { return s.minor })
	n := 0
	post := func(orgID uuid.UUID, kind string, author uuid.UUID, text string, audience string, commercial bool, likers []*student) uuid.UUID {
		n++
		id := b.id("post", n)
		published := b.now.Add(-time.Duration(1+b.intn(60*24)) * time.Hour)
		body, plain := postBody(text)
		title := text
		if r := []rune(title); len(r) > 60 {
			title = string(r[:57]) + "..."
		}
		likes, addLikes := b.likes(id, likers, published)
		defer addLikes()
		b.p.add("posts", "id", id, "org_id", orgID, "author_org_kind", kind, "author_user_id", author, "kind", "text",
			"title", title, "body_json", body, "body_text", plain, "audience_kind", audience, "audience_keys", []string{"all"},
			"audience_dim_count", 1, "commercial_flag", commercial, "status", "published", "published_at", published,
			"like_count", likes, "created_at", published.Add(-time.Hour), "updated_at", published)
		return id
	}
	for _, c := range b.clubs {
		if c.admin == nil {
			continue
		}
		for j := range 7 {
			text := clubPosts[(j+c.uni)%len(clubPosts)]
			if containsVerb(text) {
				text = fmt.Sprintf(text, c.spec.Name)
			}
			audience, likers := "public", adults
			if c.sensitive || j == 6 {
				audience, likers = "followers", c.members
			}
			post(c.orgID, "club", c.admin.id, text, audience, false, adultsOf(likers))
		}
	}
	for _, e := range b.events {
		if e.status != "published" || e.starts.Before(b.now) || e.visibility != "public" {
			continue
		}
		n++
		id := b.id("post", n)
		published := earlier(e.published.Add(2*time.Hour), b.now)
		likes, addLikes := b.likes(id, adults, published)
		b.p.add("posts", "id", id, "org_id", e.orgID, "author_org_kind", e.orgKind, "author_user_id", e.creator,
			"kind", "event_share", "title", e.title, "event_id", e.id, "audience_kind", "public", "audience_keys", []string{"all"},
			"audience_dim_count", 1, "status", "published", "published_at", published, "like_count", likes,
			"created_at", published, "updated_at", published)
		addLikes()
	}
	for k, u := range b.ref.Universities {
		for j := range 4 {
			post(u.OrgID, "university", b.reps[k], universityPosts[j%len(universityPosts)], "public", false, adults)
		}
	}
	for _, c := range b.companies {
		if c.spec.Pending {
			continue
		}
		for j := range 8 {
			text := companyPosts[j%len(companyPosts)]
			if containsVerb(text) {
				text = fmt.Sprintf(text, c.spec.Name)
			}
			post(c.orgID, "company", c.owner, text, "public", true, adults)
		}
	}
	for _, text := range platformPosts {
		post(b.ref.PlatformOrgID, "platform", b.admin1, text, "public", false, adults)
	}
	b.targetedPost(&n)
	// A draft of the main club (the manage list shows it; nobody else does).
	n++
	body, plain := postBody("Taslak: dönem sonu anketi duyurusu.")
	main := b.clubs[0]
	b.p.add("posts", "id", b.id("post", n), "org_id", main.orgID, "author_org_kind", "club", "author_user_id", main.admin.id,
		"kind", "text", "title", "Dönem sonu anketi", "body_json", body, "body_text", plain, "audience_kind", "public",
		"audience_keys", []string{"all"}, "audience_dim_count", 1, "status", "draft", "created_at", b.now, "updated_at", b.now)
}

// targetedPost is the main company's segment post to the main university's
// students: one unit of its grant spent (entitlement_usages), the audience
// fixed at creation with its rounded estimate (k ≥ 10).
func (b *builder) targetedPost(n *int) {
	c := b.companies[0]
	u := b.ref.Universities[0]
	*n++
	id := b.id("post", *n)
	usage := b.id("usage", 0)
	published := b.now.AddDate(0, 0, -3)
	b.p.add("entitlement_usages", "id", usage, "entitlement_id", c.ent, "org_id", c.orgID, "quantity", 1, "ref_type", "post",
		"ref_id", id, "actor_user_id", c.owner, "created_at", published)
	text := c.spec.Name + " " + u.Name + " öğrencilerine özel staj bilgilendirme toplantısı düzenliyor."
	body, plain := postBody(text)
	var likers []*student
	for _, s := range b.byUni[0] {
		if !s.minor {
			likers = append(likers, s)
		}
	}
	keys := []string{"uni:" + u.ID.String()}
	likes, addLikes := b.likes(id, likers, published)
	defer addLikes()
	b.p.add("posts", "id", id, "org_id", c.orgID, "author_org_kind", "company", "author_user_id", c.owner, "kind", "text",
		"title", "Staj bilgilendirme toplantısı", "body_json", body, "body_text", plain, "audience_kind", "segment",
		"audience_keys", keys, "audience_dim_count", 1,
		"audience_snapshot", jsonb(map[string]any{"uni": []string{u.ID.String()}, "segment_id": c.segment.String()}),
		"audience_size_estimate", outreach.Bucket(len(likers)), "commercial_flag", true, "status", "published", "published_at", published,
		"like_count", likes, "entitlement_usage_id", usage, "created_at", published, "updated_at", published)
}

// likes picks up to 12 likers of post and returns their number and the
// function that adds the reactions (after the post row: FK).
func (b *builder) likes(post uuid.UUID, likers []*student, published time.Time) (int, func()) {
	if len(likers) == 0 {
		return 0, func() {}
	}
	k := min(b.intn(13), len(likers))
	type like struct {
		user uuid.UUID
		at   time.Time
	}
	var ls []like
	for _, i := range b.rng.Perm(len(likers))[:k] {
		ls = append(ls, like{likers[i].id, earlier(published.Add(time.Duration(1+b.intn(48))*time.Hour), b.now)})
	}
	return k, func() {
		for _, l := range ls {
			b.p.add("post_reactions", "post_id", post, "user_id", l.user, "kind", "like", "created_at", l.at)
		}
	}
}

func adultsOf(ss []*student) []*student {
	return slices.DeleteFunc(slices.Clone(ss), func(s *student) bool { return s.minor })
}

// containsVerb reports a %s in a text template.
func containsVerb(s string) bool { return strings.Contains(s, "%s") }

// earlier is the earlier of two instants.
func earlier(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
