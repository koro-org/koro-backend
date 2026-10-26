package feed

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Register adds the 9 operations of BE-027, the 12 of BE-029 and the 9
// of BE-032 (04 §2.7) to api. svc may be nil (`koro openapi`): handlers only touch it when
// called.
func Register(api huma.API, svc Service) {
	h := handler{svc: svc}

	op.Register(api, "feed", huma.Operation{
		OperationID: "getFeed", Method: http.MethodGet, Path: "/feed",
		Summary: "Get the feed",
		Description: "Published posts the reader may see, newest first by (published_at, post_id). discover lists " +
			"every visible post, following the posts of followed and member organizations. Muted organizations and " +
			"hidden posts are left out; a minor gets no commercial post.",
	}, op.Meta{Action: string(authz.FeedFeedRead), Surface: op.SurfaceSelf}, h.getFeed)

	op.Register(api, "feed", huma.Operation{
		OperationID: "listPosts", Method: http.MethodGet, Path: "/posts",
		Summary: "List an organization's posts",
		Description: "The published posts of an organization page the caller may see (anonymous: public, " +
			"non-commercial posts only).",
	}, op.Meta{Action: string(authz.FeedPostListByOrg), Surface: op.SurfacePublic}, h.listPosts)

	op.Register(api, "feed", huma.Operation{
		OperationID: "getPost", Method: http.MethodGet, Path: "/posts/{post_id}",
		Summary: "Get a post",
		Description: "The post page behind the /posts/{post_id} links of notifications and e-mails. A reader opens " +
			"exactly the posts the feed would list them (published, in the audience, a followers post for a " +
			"follower or member of the author organization, no commercial post for a minor); an anonymous reader " +
			"the public, non-commercial ones of the organization pages. Muting the organization or hiding the post " +
			"only filters the feed: the link still opens it. Any other post (a draft, in review, hidden or removed " +
			"by moderation, out of the audience) is the same 404 as a post that does not exist. The author " +
			"organization's writers also read its drafts, posts in review and hidden posts, moderators and platform " +
			"admins every post, all in the reader view (the management view is listOrgPostsManage). The ETag is the " +
			"If-Match value of updatePost.",
	}, op.Meta{Action: string(authz.FeedPostRead), Surface: op.SurfacePublic}, h.getPost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "listOrgPostsManage", Method: http.MethodGet, Path: "/orgs/{org_id}/posts",
		Summary: "List an organization's posts for management",
		Description: "Every post of the organization, newest first: drafts, posts in review, hidden and removed " +
			"posts (removed: status only).",
	}, op.Meta{Action: string(authz.FeedPostListOrgManage), Surface: op.SurfaceOrg}, h.listOrgPosts)

	op.Register(api, "feed", huma.Operation{
		OperationID: "getPostManage", Method: http.MethodGet, Path: "/orgs/{org_id}/posts/{post_id}",
		Summary: "Get an organization's post for management",
		Description: "One post of the organization as listOrgPostsManage shows it (status, audience, commercial " +
			"label, dispatch_id; a removed post: status only), for the same people. The ETag is the If-Match value " +
			"of updatePost. A post of another organization is 404.",
	}, op.Meta{Action: string(authz.FeedPostListOrgManage), Surface: op.SurfaceOrg}, h.getOrgPost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "createPost", Method: http.MethodPost, Path: "/orgs/{org_id}/posts", DefaultStatus: http.StatusCreated,
		Summary: "Create a post (draft)",
		Description: "A public or followers post starts as a draft. A company post is always commercial. The body " +
			"is Tiptap JSON of the allowed nodes and marks (422 otherwise; an unsafe link loses its link mark). " +
			"Images are post_media uploads (createUpload) of the organization.",
	}, op.Meta{Action: string(authz.FeedPostCreate), Surface: op.SurfaceOrg, Idempotency: op.IdempotencyOptional}, h.createPost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "updatePost", Method: http.MethodPatch, Path: "/posts/{post_id}",
		Summary: "Update a post",
		Description: "If-Match with the ETag of getPost is required: missing → 428, stale → 412. A published post " +
			"keeps its kind, audience and commercial label (409 feed.published_immutable); an edit stamps " +
			"edited_at. media replaces every image.",
	}, op.Meta{Action: string(authz.FeedPostUpdate), Surface: op.SurfaceResource}, h.updatePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "publishPost", Method: http.MethodPost, Path: "/posts/{post_id}/publish",
		Summary: "Publish a draft post",
		Description: "draft → published, or pending_review for a new organization's first posts (moderator " +
			"approval). Images must be cleared by the virus scan (409 files.not_clean).",
	}, op.Meta{Action: string(authz.FeedPostPublish), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.publishPost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "deletePost", Method: http.MethodDelete, Path: "/posts/{post_id}", DefaultStatus: http.StatusNoContent,
		Summary:     "Delete a post",
		Description: "A post never published is deleted; a published one is removed (status removed).",
	}, op.Meta{Action: string(authz.FeedPostDelete), Surface: op.SurfaceResource}, h.deletePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "getPostStats", Method: http.MethodGet, Path: "/posts/{post_id}/stats",
		Summary:     "Get a post's statistics",
		Description: "Like and save counts only: who liked or saved is never shown.",
	}, op.Meta{Action: string(authz.FeedPostStats), Surface: op.SurfaceResource}, h.postStats)

	registerInteractions(api, h)
	registerModeration(api, h)
}

type handler struct{ svc Service }

// PostPath is the post_id path parameter (exported: huma reads the fields
// of an embedded struct only when its type is exported).
type PostPath struct {
	PostID string `path:"post_id" doc:"Post ID"`
}

func (p PostPath) id() uuid.UUID { return httpx.ParseID(p.PostID) }

// OrgPath is the org_id path parameter.
type OrgPath struct {
	OrgID string `path:"org_id" doc:"Organization ID"`
}

func (p OrgPath) id() uuid.UUID { return httpx.ParseID(p.OrgID) }

// FeedPage is the page of getFeed (04 §2.7: limit ≤ 50, default 20).
type FeedPage struct {
	Limit  int    `query:"limit" minimum:"1" maximum:"50" default:"20" doc:"Page size"`
	Cursor string `query:"cursor" maxLength:"512" doc:"Opaque cursor from next_cursor"`
}

const (
	sortPublished = "-published_at"
	sortID        = "-id"
)

// cursorOf decodes a published-order cursor with the request's filter
// fingerprint.
func cursorOf(raw, fp string) (*After, error) {
	c, err := page.Decode(raw, sortPublished, fp, 2)
	if err != nil || c == nil {
		return nil, err
	}
	at, err := c.Time(0)
	if err != nil {
		return nil, err
	}
	id, err := c.UUID(1)
	if err != nil {
		return nil, err
	}
	return &After{At: at, ID: id}, nil
}

type listOutput struct {
	Body PostList
}

func listOut(p PostPage, fp string) *listOutput {
	out := &listOutput{Body: PostList{Items: make([]PostDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toPostDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortPublished, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out
}

// ---- getFeed

type getFeedInput struct {
	FeedPage
	Tab  string `query:"tab" enum:"discover,following" doc:"discover (default) or following"`
	Kind string `query:"kind" enum:"text,image,event_share,opportunity_call"`
}

func (h handler) getFeed(ctx context.Context, in *getFeedInput) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	tab := in.Tab
	if tab == "" {
		tab = TabDiscover
	}
	fp := page.Fingerprint("getFeed", tab, in.Kind)
	q := FeedQuery{Tab: tab, Kind: Kind(in.Kind), Limit: in.Limit}
	if q.After, err = cursorOf(in.Cursor, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.GetFeed(ctx, a, q)
	if err != nil {
		return nil, err
	}
	return listOut(p, fp), nil
}

// ---- listPosts

type listPostsInput struct {
	page.Params
	OrgID string `query:"org_id" required:"true" doc:"The organization of the page"`
}

func (h handler) listPosts(ctx context.Context, in *listPostsInput) (*listOutput, error) {
	org, err := uuid.Parse(in.OrgID)
	if err != nil {
		return nil, problem.Field("query.org_id", "must be a UUID")
	}
	fp := page.Fingerprint("listPosts", org.String())
	q := PageQuery{Limit: in.Limit}
	if q.After, err = cursorOf(in.Cursor, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListPosts(ctx, authz.ActorOrAnonymous(ctx), org, q)
	if err != nil {
		return nil, err
	}
	return listOut(p, fp), nil
}

// ---- getPost

type postOutput struct {
	ETag string `header:"ETag"`
	Body PostDTO
}

func (h handler) getPost(ctx context.Context, in *PostPath) (*postOutput, error) {
	v, err := h.svc.GetPost(ctx, authz.ActorOrAnonymous(ctx), in.id())
	if err != nil {
		return nil, err
	}
	return &postOutput{ETag: v.ETag, Body: toPostDTO(v)}, nil
}

type manageOutput struct {
	ETag string `header:"ETag"`
	Body PostManageDTO
}

// manageOut renders the author org's view of a write.
func manageOut(v PostView) *manageOutput { return &manageOutput{ETag: v.ETag, Body: toManageDTO(v)} }

// ---- listOrgPostsManage

type listOrgPostsInput struct {
	OrgPath
	page.Params
	Status []string `query:"status,explode" maxItems:"5" enum:"draft,pending_review,published,hidden,removed" doc:"Statuses (repeat the parameter)"`
	Kind   string   `query:"kind" enum:"text,image,event_share,opportunity_call" doc:"Only this kind: status=published&kind=opportunity_call lists the calls createContactRequest's opportunity_post_id may name"`
}

type manageListOutput struct {
	Body PostManageList
}

func (h handler) listOrgPosts(ctx context.Context, in *listOrgPostsInput) (*manageListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	q := ManageQuery{Limit: in.Limit, Kind: Kind(in.Kind)}
	for _, s := range in.Status {
		q.Statuses = append(q.Statuses, Status(s))
	}
	fp := page.Fingerprint("listOrgPostsManage", in.OrgID, strings.Join(in.Status, ","), in.Kind)
	c, err := page.Decode(in.Cursor, sortID, fp, 1)
	if err != nil {
		return nil, err
	}
	if c != nil {
		id, err := c.UUID(0)
		if err != nil {
			return nil, err
		}
		q.After = &id
	}
	p, err := h.svc.ListOrgPosts(ctx, a, in.id(), q)
	if err != nil {
		return nil, err
	}
	out := &manageListOutput{Body: PostManageList{Items: make([]PostManageDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toManageDTO(v)
	}
	if p.NextID != nil {
		out.Body.NextCursor = page.Next(sortID, fp, page.UUIDVal(*p.NextID))
	}
	return out, nil
}

// ---- getPostManage

type orgPostInput struct {
	OrgPath
	PostPath
}

func (h handler) getOrgPost(ctx context.Context, in *orgPostInput) (*manageOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.GetOrgPost(ctx, a, in.OrgPath.id(), in.PostPath.id())
	if err != nil {
		return nil, err
	}
	return manageOut(v), nil
}

// ---- createPost / updatePost

// MediaBody is one image of PostWrite.
type MediaBody struct {
	FileID  string `json:"file_id" doc:"A post_media upload (createUpload) of the organization"`
	AltText string `json:"alt_text,omitempty" maxLength:"300"`
}

// OpportunityBody is the opportunity of an opportunity_call post (R-003
// §1.6; 04 §F.1).
type OpportunityBody struct {
	Type          string `json:"type" enum:"internship,job,part_time,project,volunteer,talent_call"`
	ApplyVia      string `json:"apply_via" enum:"contact_request,external_url" doc:"contact_request: company posts only"`
	ExternalURL   string `json:"external_url,omitempty" maxLength:"2048" doc:"https:// only; required for external_url"`
	Deadline      string `json:"deadline,omitempty" maxLength:"10" doc:"YYYY-MM-DD"`
	City          string `json:"city,omitempty" maxLength:"60"`
	WorkMode      string `json:"work_mode,omitempty" enum:"onsite,hybrid,remote"`
	EligibleYears []int  `json:"eligible_years,omitempty" maxItems:"8" doc:"Class years 1-8"`
}

func (b *OpportunityBody) opportunity() *Opportunity {
	if b == nil {
		return nil
	}
	return &Opportunity{Type: b.Type, ApplyVia: b.ApplyVia, ExternalURL: b.ExternalURL, Deadline: b.Deadline,
		City: b.City, WorkMode: b.WorkMode, EligibleYears: b.EligibleYears}
}

// PostWriteBody is PostWrite of 04 §2.7 with audience_kind (createPost).
type PostWriteBody struct {
	Kind           string           `json:"kind" enum:"text,image,event_share,opportunity_call"`
	Title          string           `json:"title,omitempty" maxLength:"140"`
	Body           map[string]any   `json:"body_json,omitempty" doc:"Tiptap JSON: paragraphs, headings 2-3, lists, quotes, hard breaks; bold, italic, https/mailto links"`
	EventID        string           `json:"event_id,omitempty" doc:"event_share: a published public event"`
	Opportunity    *OpportunityBody `json:"opportunity,omitempty"`
	Media          []MediaBody      `json:"media,omitempty" maxItems:"10" doc:"image posts: 1-10 images"`
	CommercialFlag bool             `json:"commercial_flag,omitempty" doc:"Sponsored content; always true for companies"`
	AudienceKind   string           `json:"audience_kind,omitempty" enum:"public,followers" doc:"Default public"`
}

type createPostInput struct {
	OrgPath
	Body PostWriteBody
}

func (h handler) createPost(ctx context.Context, in *createPostInput) (*manageOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	pi, err := in.Body.input()
	if err != nil {
		return nil, err
	}
	v, err := h.svc.CreatePost(ctx, a, in.id(), pi)
	if err != nil {
		return nil, err
	}
	return manageOut(v), nil
}

// input turns the body into the service input.
func (b PostWriteBody) input() (PostInput, error) {
	pi := PostInput{
		Kind: Kind(b.Kind), Title: b.Title, Opportunity: b.Opportunity.opportunity(), CommercialFlag: b.CommercialFlag,
		AudienceKind: AudienceKind(b.AudienceKind),
	}
	var err error
	if pi.Body, err = bodyBytes(b.Body); err != nil {
		return PostInput{}, err
	}
	if pi.EventID, err = httpx.OptID("body.event_id", b.EventID); err != nil {
		return PostInput{}, err
	}
	if pi.Media, err = mediaInputs(b.Media); err != nil {
		return PostInput{}, err
	}
	return pi, nil
}

func bodyBytes(m map[string]any) ([]byte, error) {
	if m == nil {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, problem.Field("body.body_json", "must be a JSON object")
	}
	return b, nil
}

func mediaInputs(in []MediaBody) ([]MediaInput, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]MediaInput, len(in))
	for i, m := range in {
		id, err := uuid.Parse(m.FileID)
		if err != nil {
			return nil, problem.Field("body.media.file_id", "must be a UUID")
		}
		out[i] = MediaInput{FileID: id, AltText: m.AltText}
	}
	return out, nil
}

// PostPatchBody is "PostWrite kısmi" of updatePost: an absent field stays
// as it is; media replaces every image; clear empties optional fields.
type PostPatchBody struct {
	Kind           *string          `json:"kind,omitempty" enum:"text,image,event_share,opportunity_call"`
	Title          *string          `json:"title,omitempty" maxLength:"140" doc:"Empty string clears it"`
	Body           map[string]any   `json:"body_json,omitempty" doc:"Tiptap JSON"`
	EventID        *string          `json:"event_id,omitempty"`
	Opportunity    *OpportunityBody `json:"opportunity,omitempty"`
	Media          *[]MediaBody     `json:"media,omitempty" maxItems:"10" doc:"Replaces every image; [] removes them"`
	CommercialFlag *bool            `json:"commercial_flag,omitempty"`
	AudienceKind   *string          `json:"audience_kind,omitempty" enum:"public,followers"`
	Clear          []string         `json:"clear,omitempty" maxItems:"2" enum:"title,body_json" doc:"Optional fields to empty"`
}

type updatePostInput struct {
	PostPath
	IfMatch string `header:"If-Match" doc:"ETag of getPost (required: missing → 428, stale → 412)"`
	Body    PostPatchBody
}

func (h handler) updatePost(ctx context.Context, in *updatePostInput) (*manageOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := PostPatch{Title: b.Title, Opportunity: b.Opportunity.opportunity(), CommercialFlag: b.CommercialFlag, Clear: b.Clear}
	if b.Kind != nil {
		k := Kind(*b.Kind)
		p.Kind = &k
	}
	if b.AudienceKind != nil {
		ak := AudienceKind(*b.AudienceKind)
		p.AudienceKind = &ak
	}
	if p.Body, err = bodyBytes(b.Body); err != nil {
		return nil, err
	}
	if b.EventID != nil {
		if p.EventID, err = httpx.OptID("body.event_id", *b.EventID); err != nil || p.EventID == nil {
			if err == nil {
				err = problem.Field("body.event_id", "must be a UUID")
			}
			return nil, err
		}
	}
	if b.Media != nil {
		m, err := mediaInputs(*b.Media)
		if err != nil {
			return nil, err
		}
		if m == nil {
			m = []MediaInput{}
		}
		p.Media = &m
	}
	v, err := h.svc.UpdatePost(ctx, a, in.id(), in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return manageOut(v), nil
}

// ---- publishPost / deletePost / getPostStats

type publishOutput struct {
	Body PublishResult
}

func (h handler) publishPost(ctx context.Context, in *PostPath) (*publishOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.PublishPost(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return &publishOutput{Body: PublishResult{PostID: v.Post.ID.String(), Status: string(v.Post.Status),
		PublishedAt: v.Post.PublishedAt}}, nil
}

func (h handler) deletePost(ctx context.Context, in *PostPath) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, h.svc.DeletePost(ctx, a, in.id())
}

type statsOutput struct {
	Body StatsDTO
}

func (h handler) postStats(ctx context.Context, in *PostPath) (*statsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	st, err := h.svc.PostStats(ctx, a, in.id())
	if err != nil {
		return nil, err
	}
	return &statsOutput{Body: toStatsDTO(st)}, nil
}
