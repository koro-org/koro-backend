package files_test

import (
	"context"
	"encoding/json"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
	"koro/internal/platform/queue"
)

// TestSubscribeRejected_OwnFilesOnly (T-176): the owning module's
// file.rejected subscription (queue default) hands drop only the files the
// module attached, with the purpose, the reason, the record and the
// subscription's system actor; another module's file, an unattached file
// and a payload that does not decode or names an unknown reason never reach
// drop (the last two are permanent failures).
func TestSubscribeRejected_OwnFilesOnly(t *testing.T) {
	t.Parallel()
	r := outbox.NewRegistry()
	var got []files.Rejection
	files.SubscribeRejected(r, "orgs", "orgs.drop_rejected_logo", func(_ context.Context, x files.Rejection) error {
		got = append(got, x)
		return nil
	})
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	subs := r.For(domainevent.TopicFileRejected)
	if len(subs) != 1 || subs[0].Name != "orgs.drop_rejected_logo" || subs[0].Queue != queue.Default {
		t.Fatalf("subscriptions %+v", subs)
	}
	deliver := func(p domainevent.FileRejected) error {
		t.Helper()
		env, err := domainevent.New(p, "file", p.FileID)
		if err != nil {
			t.Fatal(err)
		}
		return subs[0].Handle(t.Context(), env)
	}
	file, org := uuid.NewV7(), uuid.NewV7()
	for _, p := range []domainevent.FileRejected{
		{FileID: file, Purpose: "org_logo", Reason: "infected", AttachedModule: "events", AttachedEntityID: &org},
		{FileID: file, Purpose: "org_logo", Reason: "infected"},
		{FileID: file, Purpose: "org_logo", Reason: "infected", AttachedModule: "orgs"},
	} {
		if err := deliver(p); err != nil {
			t.Fatalf("%+v: %v", p, err)
		}
	}
	if len(got) != 0 {
		t.Fatalf("drop ran for files orgs did not attach: %+v", got)
	}
	if err := deliver(domainevent.FileRejected{FileID: file, Purpose: "org_logo", Reason: "unprocessable",
		AttachedModule: "orgs", AttachedEntityID: &org}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FileID != file || got[0].Purpose != files.PurposeOrgLogo ||
		got[0].Reason != files.RejectUnprocessable || got[0].EntityID != org ||
		got[0].Actor.Kind != authz.KindSystem || got[0].Actor.SystemJob != "orgs.drop_rejected_logo" {
		t.Fatalf("drop got %+v", got)
	}
	if m := got[0].Meta(); m["file_id"] != file.String() || m["reason"] != "unprocessable" || len(m) != 2 {
		t.Fatalf("Meta = %v", m)
	}
	err := deliver(domainevent.FileRejected{FileID: file, Purpose: "org_logo", Reason: "lost", AttachedModule: "orgs",
		AttachedEntityID: &org})
	if !queue.IsPermanent(err) {
		t.Fatalf("unknown reason: %v, want a permanent failure", err)
	}
	b, _ := json.Marshal(map[string]any{"file_id": 7})
	err = subs[0].Handle(t.Context(), domainevent.Envelope{Topic: domainevent.TopicFileRejected, Payload: b})
	if !queue.IsPermanent(err) || len(got) != 1 {
		t.Fatalf("bad payload: %v, want a permanent failure", err)
	}
}
