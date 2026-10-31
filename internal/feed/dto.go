package feed

import (
	"encoding/json"
	"time"
	"uuid"

	"koro/internal/events"
	"koro/internal/platform/httpx"
)

// AuthorDTO is the author org of a post (public org data only; the person
// who posted is never shown to readers, 04 §F.2).
type AuthorDTO struct {
	OrgID    string `json:"org_id"`
	Kind     string `json:"kind" enum:"platform,university,club,company"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	LogoURL  string `json:"logo_url,omitempty" doc:"Short-lived download URL of the author org's logo (at most 5 minutes); absent when the org has no logo or the scan has not cleared it. Do not cache past its expiry; refetch the post."`
	Verified bool   `json:"verified" doc:"True when a platform admin has verified the author org (the verification badge)"`
}

// MediaDTO is an image of a post.
type MediaDTO struct {
	FileID  string `json:"file_id"`
	URL     string `json:"url,omitempty" doc:"Short-lived download URL (at most 5 minutes); absent until the scan clears it"`
	AltText string `json:"alt_text,omitempty"`
	Width   *int   `json:"width,omitempty"`
	Height  *int   `json:"height,omitempty"`
}

// toMediaDTO is the DTO of an image of a post.
func toMediaDTO(m MediaView) MediaDTO {
	d := MediaDTO{FileID: m.FileID.String(), URL: m.URL, Width: m.Width, Height: m.Height}
	if m.AltText != nil {
		d.AltText = *m.AltText
	}
	return d
}

// EventRefDTO is the shared event of an event_share post. Org is its
// organiser (public org data, as on the event cards; BE-133), which may be
// another org than the post's author.
type EventRefDTO struct {
	EventID  string              `json:"event_id"`
	Org      *events.EventOrgDTO `json:"org,omitempty" doc:"The organiser of the event (absent only if its org cannot be read)"`
	Title    string              `json:"title"`
	Slug     string              `json:"slug" doc:"The event's public URL identifier (web: /etkinlikler/<event_id>-<slug>; BE-122)"`
	StartsAt time.Time           `json:"starts_at"`
	EndsAt   time.Time           `json:"ends_at"`
	Status   string              `json:"status" enum:"draft,published,cancelled,completed"`
}

// OpportunityDTO is the opportunity of an opportunity_call post.
type OpportunityDTO struct {
	Type          string `json:"type" enum:"internship,job,part_time,project,volunteer,talent_call"`
	ApplyVia      string `json:"apply_via" enum:"contact_request,external_url"`
	ExternalURL   string `json:"external_url,omitempty"`
	Deadline      string `json:"deadline,omitempty" doc:"YYYY-MM-DD"`
	City          string `json:"city,omitempty"`
	WorkMode      string `json:"work_mode,omitempty" enum:"onsite,hybrid,remote"`
	EligibleYears []int  `json:"eligible_years,omitempty"`
}

// toOpportunityDTO is the DTO of a stored opportunity (posts.opportunity,
// content_reports.snapshot_opportunity); nil without one.
func toOpportunityDTO(j JSON) *OpportunityDTO {
	o := decodeOpportunity(j)
	if o == nil {
		return nil
	}
	return &OpportunityDTO{Type: o.Type, ApplyVia: o.ApplyVia, ExternalURL: o.ExternalURL, Deadline: o.Deadline, City: o.City,
		WorkMode: o.WorkMode, EligibleYears: o.EligibleYears}
}

// ViewerDTO is what the signed-in reader did on the post.
type ViewerDTO struct {
	Liked           bool `json:"liked"`
	Saved           bool `json:"saved"`
	FollowingAuthor bool `json:"following_author"`
}

// PostDTO is a post as readers see it (04 §F.2): no author user, no
// audience keys, no audience estimate.
type PostDTO struct {
	PostID          string          `json:"post_id"`
	Author          AuthorDTO       `json:"author"`
	Kind            string          `json:"kind" enum:"text,image,event_share,opportunity_call"`
	Title           string          `json:"title,omitempty"`
	Body            map[string]any  `json:"body_json,omitempty" doc:"Tiptap JSON: paragraphs, headings 2-3, lists, quotes, hard breaks; bold, italic, https/mailto links"`
	Media           []MediaDTO      `json:"media"`
	Event           *EventRefDTO    `json:"event,omitempty"`
	Opportunity     *OpportunityDTO `json:"opportunity,omitempty"`
	CommercialLabel bool            `json:"commercial_label" doc:"\"Tanıtım\": commercial content"`
	PublishedAt     *time.Time      `json:"published_at,omitempty"`
	EditedAt        *time.Time      `json:"edited_at,omitempty"`
	LikeCount       int             `json:"like_count"`
	Viewer          *ViewerDTO      `json:"viewer,omitempty" doc:"Signed-in readers only"`
}

// PostList is a page of posts (getFeed, listPosts).
type PostList struct {
	Items      []PostDTO `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// PostManageDTO is a post in its author org's management view (04 §2.7):
// the reader fields plus the status, the audience and the author user. A
// removed post shows its status only.
type PostManageDTO struct {
	PostDTO
	Status               string     `json:"status" enum:"draft,pending_review,published,hidden,removed"`
	AudienceKind         string     `json:"audience_kind" enum:"public,followers,segment"`
	AudienceKeys         []string   `json:"audience_keys,omitempty"`
	AudienceSizeEstimate *int       `json:"audience_size_estimate,omitempty"`
	CommercialFlag       bool       `json:"commercial_flag"`
	AuthorUserID         string     `json:"author_user_id,omitempty"`
	SaveCount            int        `json:"save_count"`
	RemovedAt            *time.Time `json:"removed_at,omitempty"`
	ReasonCode           string     `json:"reason_code,omitempty" doc:"Moderation reason of a hidden or removed post (reported_by_users: hidden by user reports until a moderator decides)"`
	DispatchID           string     `json:"dispatch_id,omitempty" format:"uuid" doc:"A targeted post's notification dispatch (getDispatch); absent when it has none: createDispatch is then possible"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// PostManageList is a page of the management list.
type PostManageList struct {
	Items      []PostManageDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// PublishResult is the answer of publishPost (04 §2.7).
type PublishResult struct {
	PostID      string     `json:"post_id"`
	Status      string     `json:"status" enum:"published,pending_review"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// StatsDTO is the answer of getPostStats (04 §2.7). dispatch: a targeted
// post's notification dispatch (BE-035), its counts as published estimates
// (null below 10, 06 H-61).
type StatsDTO struct {
	LikeCount int                `json:"like_count"`
	SaveCount int                `json:"save_count"`
	Dispatch  *DispatchCountsDTO `json:"dispatch,omitempty" doc:"Only for a targeted post with a notification dispatch"`
}

// DispatchCountsDTO is getPostStats' dispatch (04 §2.7): the counts of
// outreach's getDispatch.
type DispatchCountsDTO struct {
	EligibleCount  *int `json:"eligible_count" doc:"Published estimate; null below 10 or before the run"`
	ConsentedCount *int `json:"consented_count" doc:"Published estimate; null below 10 or before the run"`
	SentCount      *int `json:"sent_count" doc:"Published estimate; null below 10 or before the run"`
}

func toStatsDTO(st Stats) StatsDTO {
	out := StatsDTO{LikeCount: st.LikeCount, SaveCount: st.SaveCount}
	if d := st.Dispatch; d != nil {
		out.Dispatch = &DispatchCountsDTO{EligibleCount: d.Eligible, ConsentedCount: d.Consented, SentCount: d.Sent}
	}
	return out
}

func toPostDTO(v PostView) PostDTO {
	p := v.Post
	d := PostDTO{
		PostID: p.ID.String(), Kind: string(p.Kind), CommercialLabel: p.CommercialFlag, PublishedAt: p.PublishedAt,
		EditedAt: p.EditedAt, LikeCount: p.LikeCount, Media: []MediaDTO{},
		Author: AuthorDTO{OrgID: v.Org.ID.String(), Kind: string(v.Org.Kind), Name: v.Org.Name, Slug: v.Org.Slug,
			LogoURL: v.LogoURL, Verified: v.Org.Verified},
	}
	if p.Title != nil {
		d.Title = *p.Title
	}
	if p.BodyJSON != nil {
		var body map[string]any
		if err := json.Unmarshal(p.BodyJSON, &body); err == nil {
			d.Body = body
		}
	}
	for _, m := range v.Media {
		d.Media = append(d.Media, toMediaDTO(m))
	}
	if e := v.Event; e != nil {
		d.Event = &EventRefDTO{EventID: e.ID.String(), Title: e.Title, Slug: e.Slug, StartsAt: e.StartsAt, EndsAt: e.EndsAt,
			Status: string(e.Status)}
		if o := v.EventOrg; o != nil {
			d.Event.Org = &events.EventOrgDTO{OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug}
		}
	}
	d.Opportunity = toOpportunityDTO(p.Opportunity)
	if f := v.Viewer; f != nil {
		d.Viewer = &ViewerDTO{Liked: f.Liked, Saved: f.Saved, FollowingAuthor: f.FollowingAuthor}
	}
	return d
}

func toManageDTO(v PostView) PostManageDTO {
	p := v.Post
	d := PostManageDTO{
		PostDTO: toPostDTO(v), Status: string(p.Status), AudienceKind: string(p.AudienceKind),
		AudienceSizeEstimate: p.AudienceSizeEstimate, CommercialFlag: p.CommercialFlag, SaveCount: p.SaveCount,
		RemovedAt: p.RemovedAt, ReasonCode: v.ReasonCode, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	d.Viewer = nil
	if len(p.AudienceKeys) > 0 {
		d.AudienceKeys = append([]string(nil), p.AudienceKeys...)
	}
	if p.AuthorUserID != uuid.Nil() {
		d.AuthorUserID = p.AuthorUserID.String()
	}
	d.DispatchID = httpx.IDStr(v.DispatchID)
	return d
}

// FeedOrgDTO is an org of the reader's follow and mute lists (04 §2.7
// "OrgCardDTO"): public org data only. logo_url and verified follow the
// rules of AuthorDTO (BE-094, BE-098).
type FeedOrgDTO struct {
	OrgID    string `json:"org_id"`
	Kind     string `json:"kind" enum:"platform,university,club,company,ngo"`
	Name     string `json:"name"`
	Slug     string `json:"slug"`
	LogoURL  string `json:"logo_url,omitempty" doc:"Short-lived download URL of the org's logo (at most 5 minutes); absent when the org has no logo or the scan has not cleared it. Do not cache past its expiry; refetch the list. Carried by listMyFollows and listMyMutes; absent in admin report targets."`
	Verified bool   `json:"verified" doc:"True when a platform admin has verified the org (the verification badge)"`
}

// OrgRelationDTO is the reader's own relation to an org (listMyOrgRelations).
type OrgRelationDTO struct {
	OrgID            string `json:"org_id" format:"uuid"`
	Following        bool   `json:"following"`
	Muted            bool   `json:"muted"`
	MembershipStatus string `json:"membership_status" enum:"none,pending,active" doc:"pending: a join request awaiting the club"`
}

// OrgRelationList is listMyOrgRelations.
type OrgRelationList struct {
	Items []OrgRelationDTO `json:"items" doc:"One item per requested org, in the order asked (repeated IDs once)"`
}

func toOrgRelationList(rs []OrgRelation) OrgRelationList {
	out := OrgRelationList{Items: make([]OrgRelationDTO, len(rs))}
	for i, r := range rs {
		out.Items[i] = OrgRelationDTO{OrgID: r.OrgID.String(), Following: r.Following, Muted: r.Muted,
			MembershipStatus: string(r.Membership)}
	}
	return out
}

// FeedOrgList is a page of listMyMutes.
type FeedOrgList struct {
	Items      []FeedOrgDTO `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// FollowItemDTO is a follow of listMyFollows (04 §2.7).
type FollowItemDTO struct {
	Org    FeedOrgDTO `json:"org"`
	Source string     `json:"source" enum:"manual,auto_membership,auto_university" doc:"auto_membership: came with a membership; goes when it ends"`
	Notify bool       `json:"notify" doc:"Push digest of the organization's non-commercial posts"`
}

// FollowList is a page of listMyFollows.
type FollowList struct {
	Items      []FollowItemDTO `json:"items"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

func toOrgDTO(it OrgItem) FeedOrgDTO {
	o := it.Org
	return FeedOrgDTO{OrgID: o.ID.String(), Kind: string(o.Kind), Name: o.Name, Slug: o.Slug,
		LogoURL: it.LogoURL, Verified: o.Verified}
}
