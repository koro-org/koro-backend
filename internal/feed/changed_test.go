package feed_test

import (
	"testing"
	"uuid"

	"koro/internal/feed"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/problem/problemtest"
)

// changes returns the post.changed events of post written so far
// (outbox.Mem refuses an O topic outside a transaction or through
// AfterCommit: each was written in the business transaction).
func (e *env) changes(t *testing.T, post uuid.UUID) []domainevent.PostChanged {
	t.Helper()
	var out []domainevent.PostChanged
	for _, env := range e.out.Events() {
		if env.Topic != domainevent.TopicPostChanged {
			continue
		}
		p, err := domainevent.Decode[domainevent.PostChanged](env)
		if err != nil {
			t.Fatal(err)
		}
		if p.PostID == post {
			out = append(out, p)
		}
	}
	return out
}

// wantChanges checks the post.changed sequence of post: the change names,
// the author org and the public flag of every event.
func (e *env) wantChanges(t *testing.T, post uuid.UUID, org string, public bool, want ...string) {
	t.Helper()
	got := e.changes(t, post)
	if len(got) != len(want) {
		t.Fatalf("post.changed = %+v, want %v", got, want)
	}
	for i, p := range got {
		if p.Change != want[i] || p.OrgID != orgID(org) || p.Public != public {
			t.Fatalf("post.changed[%d] = %+v, want %s (org %s, public %v)", i, p, want[i], org, public)
		}
	}
}

// TestPostChanged_ReaderVisibleChangesOnly (BE-149): every write that
// brings a post onto the public web (publication, approval, restore),
// changes it there (an edit) or takes it off (hide by a moderator or the
// report threshold, remove by a moderator, delete by the author org)
// writes post.changed; a published post cannot be turned into a
// followers, targeted or commercial one (409); a draft, a post in review
// and a hidden post that gets removed write none. Public is the anonymous
// read rule: a public, non-commercial post. (An org closing or suspended
// is org.updated: the post pages carry the org tag, BE-106.)
func TestPostChanged_ReaderVisibleChangesOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := t.Context()
	admin := persona(t, "club_admin")

	// Draft and its edits: nothing for readers.
	e.trust("club_a")
	v := e.create(t, "club_admin", "club_a", textInput("Taslak"))
	title := "Yeni başlık"
	v, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	e.wantChanges(t, v.Post.ID, "club_a", true)

	// Published, edited, hidden, restored, removed and restored again by
	// moderators, then removed by its author.
	v, err = e.svc.PublishPost(ctx, admin, v.Post.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A published post cannot leave the public web by an edit: its
	// audience and commercial label are fixed (409, no event). Leaving it
	// is a hide, a remove or a delete below.
	followersKind, commercial := feed.AudienceFollowers, true
	for _, p := range []feed.PostPatch{{AudienceKind: &followersKind}, {CommercialFlag: &commercial}} {
		_, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, v.ETag, p)
		problemtest.WantProblem(t, err, 409, "feed.published_immutable")
	}
	title = "Düzeltilmiş başlık"
	if _, err := e.svc.UpdatePost(ctx, admin, v.Post.ID, v.ETag, feed.PostPatch{Title: &title}); err != nil {
		t.Fatal(err)
	}
	e.moderate(t, "moderator", v.Post.ID, feed.ModHide, "spam")
	e.moderate(t, "moderator", v.Post.ID, feed.ModRestore, "appeal")
	e.moderate(t, "moderator", v.Post.ID, feed.ModRemove, "spam")
	e.moderate(t, "moderator", v.Post.ID, feed.ModRestore, "appeal")
	if err := e.svc.DeletePost(ctx, admin, v.Post.ID); err != nil {
		t.Fatal(err)
	}
	e.wantChanges(t, v.Post.ID, "club_a", true, "published", "edited", "hidden", "restored", "removed", "restored", "removed")

	// Hidden by the report threshold, then removed by its author: the
	// removal of a hidden post changes nothing readers see.
	reported := e.published(t, "club_admin", "club_a", textInput("Şikâyetli")).Post.ID
	for _, who := range []string{"student", "club_member", "student_minor"} {
		e.report(t, who, reported)
	}
	if err := e.svc.DeletePost(ctx, admin, reported); err != nil {
		t.Fatal(err)
	}
	e.wantChanges(t, reported, "club_a", true, "published", "hidden")

	// A followers post is not on the public web.
	in := textInput("Takipçilere")
	in.AudienceKind = feed.AudienceFollowers
	followers := e.published(t, "club_admin", "club_a", in).Post.ID
	e.wantChanges(t, followers, "club_a", false, "published")

	// A company post (always commercial) of a new org: review writes
	// nothing, the approval publishes it, a rejection in review writes
	// nothing.
	approved := e.create(t, "company_admin", "company_a", textInput("İlan"))
	if p, err := e.svc.PublishPost(ctx, persona(t, "company_admin"), approved.Post.ID); err != nil || p.Post.Status != feed.StatusPendingReview {
		t.Fatalf("PublishPost(new org) = %+v, %v", p.Post, err)
	}
	e.wantChanges(t, approved.Post.ID, "company_a", false)
	e.moderate(t, "moderator", approved.Post.ID, feed.ModApprove, "ok")
	e.wantChanges(t, approved.Post.ID, "company_a", false, "published")
	rejected := e.create(t, "company_admin", "company_a", textInput("Reddedilen"))
	if _, err := e.svc.PublishPost(ctx, persona(t, "company_admin"), rejected.Post.ID); err != nil {
		t.Fatal(err)
	}
	e.moderate(t, "moderator", rejected.Post.ID, feed.ModRemove, "spam")
	e.wantChanges(t, rejected.Post.ID, "company_a", false)

	// A draft deleted before publication: nothing.
	draft := e.create(t, "club_admin", "club_a", textInput("Silinen taslak"))
	if err := e.svc.DeletePost(ctx, admin, draft.Post.ID); err != nil {
		t.Fatal(err)
	}
	e.wantChanges(t, draft.Post.ID, "club_a", true)
}
