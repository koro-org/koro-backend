package main

import (
	"fmt"
	"slices"
	"strings"

	"koro/internal/feed"
)

// matrixFixtures seeds the feed fixtures of the matrix (03 §7.3;
// fixtures/feed.yaml): kind post (org, status, audience, author; a company
// post is commercial, commercial: true makes another one so), kind follow
// (user follows org) and kind trust (an org's feed trust level). Every post
// is dated matrixOrgTime, so an untouched post's ETag is "1hmhuo3hji000"
// (the If-Match the matrix files send).
func (w *feedWiring) matrixFixtures(fx *matrixFixtures) error {
	mem, ok := w.built.(*feed.MemStore)
	if !ok {
		return fmt.Errorf("feed: the matrix needs the in-memory store, got %T", w.built)
	}
	fixtures := fx.Of("feed")
	slices.SortStableFunc(fixtures, func(a, b fixture) int { return strings.Compare(a.Kind, b.Kind) })
	for _, f := range fixtures {
		str := func(k string) string { s, _ := f.Fields[k].(string); return s }
		switch f.Kind {
		case "post":
			id, err := fx.ID(f.Name)
			if err != nil {
				return err
			}
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			author, err := fx.Ref(f, "author")
			if err != nil {
				return err
			}
			kind := feed.AuthorKind(str("author_kind"))
			commercial, _ := f.Fields["commercial"].(bool)
			t := matrixOrgTime
			title, text := "Matris "+f.Name, "Matris gönderisi"
			p := feed.Post{
				ID: id, OrgID: org, AuthorOrgKind: kind, AuthorUserID: author, Kind: feed.KindText, Title: &title,
				BodyJSON: feed.JSON(`{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Matris gönderisi"}]}]}`),
				BodyText: &text, AudienceKind: feed.AudienceKind(str("audience")), AudienceKeys: []string{feed.KeyAll},
				AudienceDimCount: 1, CommercialFlag: commercial || kind == feed.AuthorCompany, Status: feed.Status(str("status")),
				CreatedAt: t, UpdatedAt: t,
			}
			switch p.Status {
			case feed.StatusPublished, feed.StatusHidden:
				p.PublishedAt = &t
			case feed.StatusRemoved:
				p.PublishedAt, p.RemovedAt = &t, &t
			}
			if err := mem.PutPost(p); err != nil {
				return fmt.Errorf("post %s: %w", f.Name, err)
			}
		case "follow":
			user, err := fx.Ref(f, "user")
			if err != nil {
				return err
			}
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			mem.PutFollow(user, org)
		case "trust":
			org, err := fx.Ref(f, "org")
			if err != nil {
				return err
			}
			mem.PutSettings(feed.OrgSettings{OrgID: org, TrustLevel: feed.TrustLevel(str("level"))})
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}
