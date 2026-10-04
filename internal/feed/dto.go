package feed

import (
	"encoding/json"
	"time"
	"uuid"
)

// AuthorDTO is the author org of a post (public org data only; the person
// who posted is never shown to readers, 04 §F.2).
type AuthorDTO struct {
	OrgID string `json:"org_id"`
	Kind  string `json:"kind" enum:"platform,university,club,company"`
	Name  string `json:"name"`
	Slug  string `json:"slug"`
}

// MediaDTO is an image of a post.
type MediaDTO struct {
	FileID  string `json:"file_id"`
	URL     string `json:"url,omitempty" doc:"Short-lived download URL (at most 5 minutes); absent until the scan clears it"`
	AltText string `json:"alt_text,omitempty"`
	Width   *int   `json:"width,omitempty"`
	Height  *int   `json:"height,omitempty"`
}

// EventRefDTO is the shared event of an event_share post.
type EventRefDTO struct {
	EventID  string    `json:"event_id"`
	Title    string    `json:"title"`
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Status   string    `json:"status" enum:"draft,published,cancelled,completed"`
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

// StatsDTO is the answer of getPostStats (04 §2.7). dispatch (targeted
// posts with a notification dispatch, k ≥ 10) comes with BE-032/BE-035.
type StatsDTO struct {
	LikeCount int `json:"like_count"`
	SaveCount int `json:"save_count"`
}

func toPostDTO(v PostView) PostDTO {
	p := v.Post
	d := PostDTO{
		PostID: p.ID.String(), Kind: string(p.Kind), CommercialLabel: p.CommercialFlag, PublishedAt: p.PublishedAt,
		EditedAt: p.EditedAt, LikeCount: p.LikeCount, Media: []MediaDTO{},
		Author: AuthorDTO{OrgID: v.Org.ID.String(), Kind: string(v.Org.Kind), Name: v.Org.Name, Slug: v.Org.Slug},
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
		md := MediaDTO{FileID: m.FileID.String(), URL: m.URL, Width: m.Width, Height: m.Height}
		if m.AltText != nil {
			md.AltText = *m.AltText
		}
		d.Media = append(d.Media, md)
	}
	if e := v.Event; e != nil {
		d.Event = &EventRefDTO{EventID: e.ID.String(), Title: e.Title, StartsAt: e.StartsAt, EndsAt: e.EndsAt,
			Status: string(e.Status)}
	}
	if o := decodeOpportunity(p.Opportunity); o != nil {
		d.Opportunity = &OpportunityDTO{Type: o.Type, ApplyVia: o.ApplyVia, ExternalURL: o.ExternalURL,
			Deadline: o.Deadline, City: o.City, WorkMode: o.WorkMode, EligibleYears: o.EligibleYears}
	}
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
		RemovedAt: p.RemovedAt, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	d.Viewer = nil
	if len(p.AudienceKeys) > 0 {
		d.AudienceKeys = append([]string(nil), p.AudienceKeys...)
	}
	if p.AuthorUserID != uuid.Nil() {
		d.AuthorUserID = p.AuthorUserID.String()
	}
	return d
}
