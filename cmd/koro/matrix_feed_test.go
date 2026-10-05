package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"koro/internal/authz/authztest"
	"koro/internal/feed"
	"koro/internal/students"
)

// matrixFixtures seeds the feed fixtures of the matrix (03 §7.3;
// fixtures/feed.yaml): kind post (org, status, audience, author; a company
// post is commercial, commercial: true makes another one so), kind follow
// (user follows org), kind trust (an org's feed trust level), kind report
// (BE-032: reporter, target_type, target, reason) and kind audience
// (BE-032: count eligible student profiles holding keys — the uni: keys of
// the named universities — in students' store, so a targeted post reaches
// k ≥ 10). Every post is dated matrixOrgTime, so an untouched post's ETag
// is "1hmhuo3hji000" (the If-Match the matrix files send).
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
		case "report":
			id, err := fx.ID(f.Name)
			if err != nil {
				return err
			}
			reporter, err := fx.Ref(f, "reporter")
			if err != nil {
				return err
			}
			target, err := fx.Ref(f, "target")
			if err != nil {
				return err
			}
			t := matrixOrgTime
			if err := mem.PutReport(feed.Report{ID: id, ReporterUserID: reporter, TargetType: feed.TargetType(str("target_type")),
				TargetID: target, Reason: feed.ReportReason(str("reason")), Status: feed.ReportOpen, CreatedAt: t,
				UpdatedAt: t}); err != nil {
				return fmt.Errorf("report %s: %w", f.Name, err)
			}
		case "audience":
			if err := w.matrixAudience(fx, f); err != nil {
				return err
			}
		default:
			return fmt.Errorf("fixture %s: unknown kind %q", f.Name, f.Kind)
		}
	}
	return nil
}

// matrixAudience seeds count eligible student profiles of the first named
// university holding the uni: keys of the named universities (students'
// in-memory store; deterministic user IDs).
func (w *feedWiring) matrixAudience(fx *matrixFixtures, f fixture) error {
	if w.late == nil || w.late.students.store == nil {
		return fmt.Errorf("fixture %s: students is not built", f.Name)
	}
	unis, err := matrixIDs(fx, f, "unis")
	if err != nil || len(unis) == 0 {
		return fmt.Errorf("fixture %s: unis must name universities: %w", f.Name, err)
	}
	n, _ := f.Fields["count"].(int)
	keys := make([]string, len(unis))
	for i, u := range unis {
		keys[i] = "uni:" + u.String()
	}
	ctx := context.Background()
	st := w.late.students.store
	for i := range n {
		user := authztest.ID(f.Name + "_" + strconv.Itoa(i))
		if _, _, err := st.InsertProfile(ctx, students.Profile{UserID: user, UniversityID: unis[0],
			DegreeLevel: students.DegreeBachelor, Status: students.ProfileActive}); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
		if err := st.SetAudience(ctx, user, keys, true, matrixOrgTime); err != nil {
			return fmt.Errorf("fixture %s: %w", f.Name, err)
		}
	}
	return nil
}
