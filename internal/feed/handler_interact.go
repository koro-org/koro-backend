package feed

import (
	"context"
	"fmt"
	"net/http"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// registerInteractions adds the operations of BE-029 (04 §2.7, §F.3,
// §F.5), BE-122 and BE-145: likes, saves, hides, follows and mutes of the
// signed-in reader.
func registerInteractions(api huma.API, h handler) {
	op.Register(api, "feed", huma.Operation{
		OperationID: "likePost", Method: http.MethodPut, Path: "/posts/{post_id}/like", DefaultStatus: http.StatusNoContent,
		Summary: "Like a post",
		Description: "Idempotent: a repeated like counts once. The post must be a published post the reader may see " +
			"(404 otherwise). The author organization sees the like count only, never who liked.",
	}, op.Meta{Action: string(authz.FeedReactionManage), Surface: op.SurfaceResource}, h.likePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "unlikePost", Method: http.MethodDelete, Path: "/posts/{post_id}/like", DefaultStatus: http.StatusNoContent,
		Summary:     "Remove a like",
		Description: "Idempotent: 204 whether or not the reader had liked the post.",
	}, op.Meta{Action: string(authz.FeedReactionManage), Surface: op.SurfaceResource}, h.unlikePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "savePost", Method: http.MethodPut, Path: "/posts/{post_id}/save", DefaultStatus: http.StatusNoContent,
		Summary: "Save a post",
		Description: "Idempotent. The post must be a published post the reader may see (404 otherwise). The author " +
			"organization sees the save count only.",
	}, op.Meta{Action: string(authz.FeedSaveManage), Surface: op.SurfaceResource}, h.savePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "unsavePost", Method: http.MethodDelete, Path: "/posts/{post_id}/save", DefaultStatus: http.StatusNoContent,
		Summary:     "Remove a saved post",
		Description: "Idempotent: 204 whether or not the reader had saved the post.",
	}, op.Meta{Action: string(authz.FeedSaveManage), Surface: op.SurfaceResource}, h.unsavePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "hidePost", Method: http.MethodPost, Path: "/posts/{post_id}/hide", DefaultStatus: http.StatusNoContent,
		Summary: "Hide a post from my feed",
		Description: "The post leaves the reader's feed (it stays on the organization page). Idempotent. The post " +
			"must be a published post the reader may see (404 otherwise).",
	}, op.Meta{Action: string(authz.FeedHideCreate), Surface: op.SurfaceResource, Idempotency: op.IdempotencyOptional}, h.hidePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "unhidePost", Method: http.MethodDelete, Path: "/posts/{post_id}/hide", DefaultStatus: http.StatusNoContent,
		Summary: "Show a hidden post in my feed again",
		Description: "Undoes hidePost. Idempotent: 204 whether or not the reader had hidden the post. On a post the " +
			"reader no longer sees only their own hide goes (no hide: 404, as for a missing post).",
	}, op.Meta{Action: string(authz.FeedHideDelete), Surface: op.SurfaceResource}, h.unhidePost)

	op.Register(api, "feed", huma.Operation{
		OperationID: "listMySavedPosts", Method: http.MethodGet, Path: "/me/saved-posts",
		Summary: "List my saved posts",
		Description: "Newest save first. A saved post the reader may no longer see (removed, hidden by a moderator, " +
			"out of the audience) is skipped.",
	}, op.Meta{Action: string(authz.FeedSaveListOwn), Surface: op.SurfaceSelf}, h.listSaved)

	op.Register(api, "feed", huma.Operation{
		OperationID: "listMyHiddenPosts", Method: http.MethodGet, Path: "/me/hidden-posts",
		Summary: "List the posts I hid from my feed",
		Description: "Newest hide first; unhidePost brings one back. A hidden post the reader may no longer see " +
			"(removed, hidden by a moderator, out of the audience) is skipped, as in listMySavedPosts: it would not " +
			"reach the feed anyway. Posts of a muted organization are listed (a mute is separate).",
	}, op.Meta{Action: string(authz.FeedHideListOwn), Surface: op.SurfaceSelf}, h.listHidden)

	op.Register(api, "feed", huma.Operation{
		OperationID: "followOrg", Method: http.MethodPut, Path: "/me/follows/{org_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Follow an organization",
		Description: "Idempotent. notify asks for a push digest of the organization's non-commercial posts (absent: " +
			"unchanged). An automatic follow (membership) becomes the reader's own. The organization never sees who " +
			"follows it.",
	}, op.Meta{Action: string(authz.FeedFollowManage), Surface: op.SurfaceSelf}, h.followOrg)

	op.Register(api, "feed", huma.Operation{
		OperationID: "unfollowOrg", Method: http.MethodDelete, Path: "/me/follows/{org_id}", DefaultStatus: http.StatusNoContent,
		Summary:     "Unfollow an organization",
		Description: "Idempotent: 204 whether or not the reader followed the organization.",
	}, op.Meta{Action: string(authz.FeedFollowManage), Surface: op.SurfaceSelf}, h.unfollowOrg)

	op.Register(api, "feed", huma.Operation{
		OperationID: "listMyFollows", Method: http.MethodGet, Path: "/me/follows",
		Summary:     "List the organizations I follow",
		Description: "Newest first. source: manual, or auto_membership for a follow that came with a membership.",
	}, op.Meta{Action: string(authz.FeedFollowListOwn), Surface: op.SurfaceSelf}, h.listFollows)

	op.Register(api, "feed", huma.Operation{
		OperationID: "muteOrg", Method: http.MethodPut, Path: "/me/mutes/{org_id}", DefaultStatus: http.StatusNoContent,
		Summary: "Mute an organization",
		Description: "The organization's posts leave the reader's feed (the feed's block); its page stays readable. " +
			"Idempotent. Contact request blocks are separate (messaging).",
	}, op.Meta{Action: string(authz.FeedMuteManage), Surface: op.SurfaceSelf}, h.muteOrg)

	op.Register(api, "feed", huma.Operation{
		OperationID: "unmuteOrg", Method: http.MethodDelete, Path: "/me/mutes/{org_id}", DefaultStatus: http.StatusNoContent,
		Summary:     "Unmute an organization",
		Description: "Idempotent: 204 whether or not the reader had muted the organization.",
	}, op.Meta{Action: string(authz.FeedMuteManage), Surface: op.SurfaceSelf}, h.unmuteOrg)

	op.Register(api, "feed", huma.Operation{
		OperationID: "listMyMutes", Method: http.MethodGet, Path: "/me/mutes",
		Summary:     "List the organizations I muted",
		Description: "Newest first.",
	}, op.Meta{Action: string(authz.FeedMuteListOwn), Surface: op.SurfaceSelf}, h.listMutes)

	op.Register(api, "feed", huma.Operation{
		OperationID: "listMyOrgRelations", Method: http.MethodGet, Path: "/me/org-relations",
		Summary: "Get my relations to organizations",
		Description: "The signed-in reader's own follow, mute and membership state of each organization asked " +
			"(the buttons of an org page or card; BE-122). The public organization reads (getOrgBySlug, getClub, " +
			"listClubs, searchOrgs) never carry the reader's state, so their anonymous responses stay cacheable; " +
			"clients read this alongside, outside any shared cache. Every ID gets an item, existing or not: an " +
			"unknown organization is following=false, muted=false, membership_status=none.",
	}, op.Meta{Action: string(authz.FeedFollowListOwn), Surface: op.SurfaceSelf}, h.listOrgRelations)
}

// postAction runs one reader action on a post.
func (h handler) postAction(ctx context.Context, in *PostPath, fn func(context.Context, authz.Actor, uuid.UUID) error) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, fn(ctx, a, in.id())
}

func (h handler) likePost(ctx context.Context, in *PostPath) (*struct{}, error) {
	return h.postAction(ctx, in, h.svc.LikePost)
}

func (h handler) unlikePost(ctx context.Context, in *PostPath) (*struct{}, error) {
	return h.postAction(ctx, in, h.svc.UnlikePost)
}

func (h handler) savePost(ctx context.Context, in *PostPath) (*struct{}, error) {
	return h.postAction(ctx, in, h.svc.SavePost)
}

func (h handler) unsavePost(ctx context.Context, in *PostPath) (*struct{}, error) {
	return h.postAction(ctx, in, h.svc.UnsavePost)
}

func (h handler) hidePost(ctx context.Context, in *PostPath) (*struct{}, error) {
	return h.postAction(ctx, in, h.svc.HidePost)
}

func (h handler) unhidePost(ctx context.Context, in *PostPath) (*struct{}, error) {
	return h.postAction(ctx, in, h.svc.UnhidePost)
}

// The cursor sorts of the reader's own post lists; the fingerprint is the
// operationId, so a cursor of one list is refused by the other.
const (
	sortSaved  = "-saved_at"
	sortHidden = "-hidden_at"
)

func (h handler) listSaved(ctx context.Context, in *FeedPage) (*listOutput, error) {
	return h.listMarked(ctx, in, "listMySavedPosts", sortSaved, h.svc.ListSavedPosts)
}

func (h handler) listHidden(ctx context.Context, in *FeedPage) (*listOutput, error) {
	return h.listMarked(ctx, in, "listMyHiddenPosts", sortHidden, h.svc.ListHiddenPosts)
}

// listMarked answers one of the reader's own post lists.
func (h handler) listMarked(ctx context.Context, in *FeedPage, opID, sort string,
	list func(context.Context, authz.Actor, PageQuery) (PostPage, error),
) (*listOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint(opID)
	q := PageQuery{Limit: in.Limit}
	if q.After, err = keysetOf(in.Cursor, sort, fp); err != nil {
		return nil, err
	}
	p, err := list(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &listOutput{Body: PostList{Items: make([]PostDTO, len(p.Items))}}
	for i, v := range p.Items {
		out.Body.Items[i] = toPostDTO(v)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sort, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

// keysetOf decodes a (time, id) cursor of sort with the request's filter
// fingerprint.
func keysetOf(raw, sort, fp string) (*After, error) {
	c, err := page.Decode(raw, sort, fp, 2)
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

type followInput struct {
	OrgPath
	Body *struct {
		Notify *bool `json:"notify,omitempty" doc:"Push digest of the organization's non-commercial posts (absent: unchanged)"`
	}
}

func (h handler) followOrg(ctx context.Context, in *followInput) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var notify *bool
	if in.Body != nil {
		notify = in.Body.Notify
	}
	return nil, h.svc.FollowOrg(ctx, a, in.id(), notify)
}

// orgAction runs one reader action on an org.
func (h handler) orgAction(ctx context.Context, in *OrgPath, fn func(context.Context, authz.Actor, uuid.UUID) error) (*struct{}, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	return nil, fn(ctx, a, in.id())
}

func (h handler) unfollowOrg(ctx context.Context, in *OrgPath) (*struct{}, error) {
	return h.orgAction(ctx, in, h.svc.UnfollowOrg)
}

func (h handler) muteOrg(ctx context.Context, in *OrgPath) (*struct{}, error) {
	return h.orgAction(ctx, in, h.svc.MuteOrg)
}

func (h handler) unmuteOrg(ctx context.Context, in *OrgPath) (*struct{}, error) {
	return h.orgAction(ctx, in, h.svc.UnmuteOrg)
}

const sortCreated = "-created_at"

type followsOutput struct {
	Body FollowList
}

func (h handler) listFollows(ctx context.Context, in *page.Params) (*followsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyFollows")
	q := PageQuery{Limit: in.Limit}
	if q.After, err = keysetOf(in.Cursor, sortCreated, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListFollows(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &followsOutput{Body: FollowList{Items: make([]FollowItemDTO, 0, len(p.Items))}}
	for _, it := range p.Items {
		d := FollowItemDTO{Org: toOrgDTO(it)}
		if f := it.Follow; f != nil {
			d.Source, d.Notify = string(f.Source), f.Notify
		}
		out.Body.Items = append(out.Body.Items, d)
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortCreated, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

type mutesOutput struct {
	Body FeedOrgList
}

func (h handler) listMutes(ctx context.Context, in *page.Params) (*mutesOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	fp := page.Fingerprint("listMyMutes")
	q := PageQuery{Limit: in.Limit}
	if q.After, err = keysetOf(in.Cursor, sortCreated, fp); err != nil {
		return nil, err
	}
	p, err := h.svc.ListMutes(ctx, a, q)
	if err != nil {
		return nil, err
	}
	out := &mutesOutput{Body: FeedOrgList{Items: make([]FeedOrgDTO, 0, len(p.Items))}}
	for _, it := range p.Items {
		out.Body.Items = append(out.Body.Items, toOrgDTO(it))
	}
	if p.Next != nil {
		out.Body.NextCursor = page.Next(sortCreated, fp, page.TimeVal(p.Next.At), page.UUIDVal(p.Next.ID))
	}
	return out, nil
}

type orgRelationsInput struct {
	OrgID []string `query:"org_id,explode" required:"true" minItems:"1" maxItems:"50" doc:"Organization IDs (repeat the parameter)"`
}

type orgRelationsOutput struct {
	Body OrgRelationList
}

func (h handler) listOrgRelations(ctx context.Context, in *orgRelationsInput) (*orgRelationsOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(in.OrgID))
	for i, raw := range in.OrgID {
		if ids[i], err = uuid.Parse(raw); err != nil {
			return nil, problem.Field(fmt.Sprintf("query.org_id[%d]", i), "must be a UUID")
		}
	}
	rs, err := h.svc.OrgRelations(ctx, a, ids)
	if err != nil {
		return nil, err
	}
	return &orgRelationsOutput{Body: toOrgRelationList(rs)}, nil
}
