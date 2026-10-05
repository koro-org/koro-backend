package main

import (
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz/authztest"
	"koro/internal/feed"
	"koro/internal/notify"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/privacy"
	"koro/internal/platform/queue"
)

// useMemStore installs the in-memory feed stores used by service,
// wiring and authz matrix tests (05 §13.2). First owner: BE-027.
// Only tickets of the feed module edit this file (BE-001 R4).
func (w *feedWiring) useMemStore() {
	w.tx = db.NopTx{}
	w.newStore = func(c *container) feed.Store { return feed.NewMemStore(c.infra.clock.Now) }
}

// TestWire_FeedProvidesServiceAndPrivacy pins what feed binds in build:
// its service (feed.Posts for messaging, answering from the built store),
// its KVKK provider and the post.published subscription.
func TestWire_FeedProvidesServiceAndPrivacy(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if c.feed.svc == nil || c.feed.built == nil {
		t.Fatal("feed service not built")
	}
	if !slices.ContainsFunc(c.privacy, func(p privacy.Provider) bool { return p.Name() == "feed" }) {
		t.Fatal("feed privacy provider not registered")
	}
	ctx := t.Context()
	if _, err := c.feed.svc.Get(ctx, uuid.NewV7()); err == nil {
		t.Fatal("Get(unknown) found a post")
	}
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	title := "Kanca"
	p := feed.Post{
		ID: uuid.NewV7(), OrgID: authztest.ID("club_a"), AuthorOrgKind: feed.AuthorClub, AuthorUserID: authztest.ID("club_admin"),
		Kind: feed.KindText, Title: &title, BodyJSON: feed.JSON(`{"type":"doc"}`), AudienceKind: feed.AudiencePublic,
		AudienceKeys: []string{feed.KeyAll}, AudienceDimCount: 1, Status: feed.StatusDraft, CreatedAt: at, UpdatedAt: at,
	}
	if err := c.feed.built.(*feed.MemStore).PutPost(p); err != nil {
		t.Fatal(err)
	}
	got, err := c.feed.svc.Get(ctx, p.ID)
	if err != nil || got.ID != p.ID || got.OrgID != p.OrgID || got.Kind != feed.KindText || got.Title != title {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if subs := c.infra.subs.For(domainevent.TopicPostPublished); !slices.ContainsFunc(subs, func(s outbox.Subscription) bool {
		return s.Name == feed.NotifyFollowersTaskName
	}) {
		t.Fatalf("post.published subscribers = %v", subs)
	}
}

// TestWire_FeedFollowsMembershipsAndReconciles pins what BE-029 binds: the
// membership and club sensitivity consumers, the hourly counter job and
// directory as the sensitive club source.
func TestWire_FeedFollowsMembershipsAndReconciles(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	for topic, task := range map[string]string{
		domainevent.TopicMembershipStarted:      feed.AutoFollowTaskName,
		domainevent.TopicMembershipEnded:        feed.RemoveAutoFollowTaskName,
		domainevent.TopicClubSensitivityChanged: feed.DropSensitiveAutoFollowsTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == task }) {
			t.Errorf("%s has no %s subscriber", topic, task)
		}
	}
	var jobs []string
	for _, j := range c.infra.jobs.Jobs() {
		if strings.HasPrefix(j.Name, "feed.") {
			jobs = append(jobs, j.Name+" "+j.Cron+" "+j.Queue)
		}
	}
	if want := []string{"feed.reconcile_counters 35 * * * * low"}; !slices.Equal(jobs, want) {
		t.Fatalf("feed jobs = %v, want %v", jobs, want)
	}
}

// TestWire_FeedModerationNotices pins what BE-032 binds: the moderation
// notice consumers (post.submitted_for_review, content_report.created,
// post.moderated), the plain notice tasks, and the moderation staff lookup
// (moderators from identity's store, platform admins from the notify hook
// identity fills; T-249).
func TestWire_FeedModerationNotices(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	for topic, task := range map[string]string{
		domainevent.TopicPostSubmittedForReview: feed.NotifyReviewQueueTaskName,
		domainevent.TopicContentReportCreated:   feed.NotifyReportCreatedTaskName,
		domainevent.TopicPostModerated:          feed.NotifyPostModeratedTaskName,
	} {
		if !slices.ContainsFunc(c.infra.subs.For(topic), func(s outbox.Subscription) bool { return s.Name == task }) {
			t.Errorf("%s has no %s subscriber", topic, task)
		}
	}
	for _, task := range []string{feed.NotifyOrgModeratedTaskName, feed.NotifyReportResolvedTaskName} {
		if !slices.ContainsFunc(c.infra.jobs.Tasks(), func(x queue.Task) bool { return x.Name == task }) {
			t.Errorf("task %s is not registered", task)
		}
	}
	staff := feedStaff{c: c}
	if mods, err := staff.Moderators(t.Context()); err != nil || len(mods) != 0 {
		t.Fatalf("Moderators (empty identity store) = %v, %v", mods, err)
	}
	if _, err := staff.PlatformAdmins(t.Context()); err != nil {
		t.Fatalf("PlatformAdmins: %v", err)
	}
	if _, err := (feedSender{c: &container{}}).Send(t.Context(), notify.Message{}); err == nil {
		t.Fatal("a sender without notify succeeded")
	}
}
