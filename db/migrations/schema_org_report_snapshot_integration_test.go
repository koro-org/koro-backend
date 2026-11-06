//go:build integration

package migrations_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/db"
	"koro/internal/platform/testutil"
)

// TestSchema_OrgReportSnapshot (BE-180, migration 000180): an org report
// keeps the org's profile, and a logo replaced while reported stays:
//   - the CHECKs: each snapshot fits its target (a post's columns on a post
//     report, an org's on an org report, none on a message report), the
//     columns need snapshot_at, an org snapshot names the org, and the limits
//     are the org's; an org report without a snapshot (filed before BE-180)
//     stays valid;
//   - content_reports_snapshot_guard covers the org columns: any change is
//     refused, an UPDATE of the status passes;
//   - org_dropped_logos: a file is kept once, the row needs its file and org,
//     and goes with the file (the scan may still reject it);
//   - files_retired_public_chk: a retired file has no public copy;
//   - the policy and the text of content_report;
//   - the down migration drops all of it, clears the org snapshots (000168's
//     CHECK allows a post report's only) and restores the text; the up one
//     brings it back.
func TestSchema_OrgReportSnapshot(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	const at = "2026-11-06T12:00:00Z"
	const ins = `INSERT INTO content_reports (id, reporter_user_id, target_type, target_id, reason, snapshot_title,
		snapshot_org_name, snapshot_org_description, snapshot_org_website_url, snapshot_org_social_links, snapshot_org_logo_file_id, snapshot_at)
		VALUES ($1, $2, $3, $4, 'spam', $5, $6, $7, $8, $9, $10, $11)`
	check := func(t *testing.T) uuid.UUID {
		t.Helper()
		club, author := w.org("club"), w.user()
		post := w.post(club, "club", author, nil)
		report := uuid.NewV7()
		w.exec(ins, report, w.user(), "org", club, nil, "Kulüp", "Açıklama", "https://a.koro.test",
			`{"instagram": "https://i.koro.test/a"}`, uuid.NewV7(), at)
		// An org without the optional fields: its name and the time. No snapshot: before BE-180.
		w.exec(ins, uuid.NewV7(), w.user(), "org", club, nil, "Kulüp", nil, nil, nil, nil, at)
		w.exec(ins, uuid.NewV7(), w.user(), "org", club, nil, nil, nil, nil, nil, nil, nil)
		bad := func(name, target string, title, orgName, description, website, links, logo, snapAt any, constraint string) p1aCase {
			id := post
			if target == "org" {
				id = club
			}
			return p1aCase{name, ins, []any{uuid.NewV7(), w.user(), target, id, title, orgName, description, website, links, logo, snapAt},
				db.CodeCheckViolation, constraint}
		}
		frozen := func(name, sql string) p1aCase {
			return p1aCase{name, sql, []any{report}, db.CodeRestrictViolation, "content_reports_snapshot_frozen"}
		}
		expectFail(t, w.d.Pool, []p1aCase{
			bad("a message report with a snapshot", "message", nil, nil, nil, nil, nil, nil, at, "content_reports_snapshot_target_chk"),
			bad("an org column on a post report", "post", nil, "Kulüp", nil, nil, nil, nil, at, "content_reports_snapshot_org_columns_chk"),
			bad("a post column on an org report", "org", "Başlık", "Kulüp", nil, nil, nil, nil, at, "content_reports_snapshot_post_columns_chk"),
			bad("an org snapshot without the name", "org", nil, nil, "Açıklama", nil, nil, nil, at, "content_reports_snapshot_org_name_required_chk"),
			bad("a name without its time", "org", nil, "Kulüp", nil, nil, nil, nil, nil, "content_reports_snapshot_at_chk"),
			bad("a logo without its time", "org", nil, nil, nil, nil, nil, uuid.NewV7(), nil, "content_reports_snapshot_at_chk"),
			bad("a name of 201 characters", "org", nil, repeat("ş", 201), nil, nil, nil, nil, at, "content_reports_snapshot_org_name_chk"),
			bad("a description of 5001 characters", "org", nil, "Kulüp", repeat("ş", 5001), nil, nil, nil, at, "content_reports_snapshot_org_description_chk"),
			bad("a website of 2049 characters", "org", nil, "Kulüp", nil, repeat("a", 2049), nil, nil, at, "content_reports_snapshot_org_website_url_chk"),
			bad("links that are no object", "org", nil, "Kulüp", nil, nil, `[1]`, nil, at, "content_reports_snapshot_org_social_links_chk"),
			frozen("a new name", `UPDATE content_reports SET snapshot_org_name = 'Yeni' WHERE id = $1`),
			frozen("a cleared description", `UPDATE content_reports SET snapshot_org_description = NULL WHERE id = $1`),
			frozen("a new website", `UPDATE content_reports SET snapshot_org_website_url = 'https://b.koro.test' WHERE id = $1`),
			frozen("new links", `UPDATE content_reports SET snapshot_org_social_links = '{}' WHERE id = $1`),
			frozen("another logo", `UPDATE content_reports SET snapshot_org_logo_file_id = uuidv7() WHERE id = $1`),
		})
		// The moderation flow moves the row; the snapshot stays.
		w.exec(`UPDATE content_reports SET status = 'dismissed', resolved_at = now() WHERE id = $1`, report)
		if n := w.count(`SELECT count(*) FROM content_reports WHERE id = $1 AND status = 'dismissed' AND snapshot_org_name = 'Kulüp'
			AND snapshot_org_description = 'Açıklama' AND snapshot_org_website_url = 'https://a.koro.test'
			AND snapshot_org_social_links->>'instagram' = 'https://i.koro.test/a' AND snapshot_org_logo_file_id IS NOT NULL`, report); n != 1 {
			t.Fatal("the org snapshot changed with the moderation flow")
		}

		// The kept logos.
		file := w.file("org_logo")
		const keep = `INSERT INTO org_dropped_logos (file_id, org_id, dropped_at) VALUES ($1, $2, now())`
		w.exec(keep, file, club)
		expectFail(t, w.d.Pool, []p1aCase{
			{"the same file twice", keep, []any{file, club}, db.CodeUniqueViolation, "org_dropped_logos_pkey"},
			{"unknown file", keep, []any{uuid.NewV7(), club}, db.CodeForeignKeyViolation, "org_dropped_logos_file_id_fkey"},
			{"unknown org", keep, []any{w.file("org_logo"), uuid.NewV7()}, db.CodeForeignKeyViolation, "org_dropped_logos_org_id_fkey"},
		})
		w.exec(`DELETE FROM files WHERE id = $1`, file)
		if n := w.count(`SELECT count(*) FROM org_dropped_logos WHERE file_id = $1`, file); n != 0 {
			t.Fatal("a kept logo's row outlived its file")
		}

		// A retired file has no public copy; the column alone is free.
		w.exec(`UPDATE files SET retired_at = now() WHERE id = $1`, w.file("org_logo"))
		retired := uuid.NewV7()
		expectFail(t, w.d.Pool, []p1aCase{
			{"a public copy of a retired file", `INSERT INTO files (id, purpose, bucket, storage_key, declared_mime, size_bytes, sha256,
				status, scan_status, public_key, retired_at)
				VALUES ($1, 'org_logo', 'koro-files-dev', $2, 'image/png', 10, decode(repeat('ab', 32), 'hex'), 'active', 'clean', $3, now())`,
				[]any{retired, "org_logo/2026/11/" + retired.String(), "org_logo/" + retired.String() + "-" + repeat("ab", 16) + ".png"},
				db.CodeCheckViolation, "files_retired_public_chk"},
		})

		if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'org_logo_dropped' AND target = 'org_dropped_logos' AND owner_module = 'feed' AND NOT legal_approved`); n != 1 {
			t.Fatal("no org_logo_dropped retention policy")
		}
		if n := w.count(`SELECT count(*) FROM retention_policies WHERE code = 'content_report' AND description_tr LIKE '%kuruluş profiliyle%'`); n != 1 {
			t.Fatal("the content_report policy does not name the org's copy")
		}
		return club
	}
	club := check(t)

	owner := testutil.OwnerConn(t, w.d)
	if _, err := owner.Exec(t.Context(), mustRead(t, "000180_snapshot_reported_orgs.down.sql")); err != nil {
		t.Fatal(err)
	}
	if n := w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'content_reports' AND column_name LIKE 'snapshot\_org\_%'`) +
		w.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'files' AND column_name = 'retired_at'`) +
		w.count(`SELECT count(*) FROM pg_class WHERE relname = 'org_dropped_logos'`) +
		w.count(`SELECT count(*) FROM retention_policies WHERE code = 'org_logo_dropped'`) +
		w.count(`SELECT count(*) FROM retention_policies WHERE code = 'content_report' AND description_tr LIKE '%kuruluş profiliyle%'`) +
		w.count(`SELECT count(*) FROM content_reports WHERE target_type = 'org' AND target_id = $1 AND snapshot_at IS NOT NULL`, club); n != 0 {
		t.Fatalf("down left %d objects of 000180", n)
	}
	// 000168's rules are back: no snapshot on an org report, and the guard still holds.
	expectFail(t, w.d.Pool, []p1aCase{
		{"an org report with a snapshot", `INSERT INTO content_reports (id, reporter_user_id, target_type, target_id, reason, snapshot_at)
			VALUES ($1, $2, 'org', $3, 'spam', now())`, []any{uuid.NewV7(), w.user(), club}, db.CodeCheckViolation, "content_reports_snapshot_target_chk"},
	})
	if _, err := owner.Exec(t.Context(), mustRead(t, "000180_snapshot_reported_orgs.up.sql")); err != nil {
		t.Fatal(err)
	}
	check(t)
}
