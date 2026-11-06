//go:build integration

package migrations_test

import (
	"testing"

	"koro/internal/platform/testutil"
)

// TestSchema_RetentionPolicyDescriptions (000178; BE-176, DOC-003 finding 3):
// the DPO's texts name what the code does — content_report the message copy
// of 000174, dsr_export_file the start of its period (the completion of the
// request, kvkk.purge_exports). Only the seeded, unapproved rows change;
// period, anchor and method stay; the down migration restores the texts of
// 000168 and 000090 word for word and leaves an approved row alone.
func TestSchema_RetentionPolicyDescriptions(t *testing.T) {
	t.Parallel()
	w := newP1aWorld(t)
	owner := testutil.OwnerConn(t, w.d)
	exec := func(name string) {
		t.Helper()
		if _, err := owner.Exec(t.Context(), mustRead(t, name)); err != nil {
			t.Fatal(err)
		}
	}
	text := func(code string) string {
		t.Helper()
		return queryString(t, w.d, `SELECT description_tr FROM retention_policies WHERE code = $1`, code)
	}
	const (
		reportOld = "Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle birlikte silinir."
		reportNew = "Çözülmüş içerik şikâyetleri, şikâyet anında saklanan gönderi metniyle ve mesaj şikâyetinde saklanan mesaj kopyasıyla (şikâyet edilen mesaj ve öncesindeki en çok 5 mesaj) birlikte silinir."
		exportOld = "İlgili kişi başvurusu dışa aktarma dosyası silinir."
		exportNew = "İlgili kişi başvurusu dışa aktarma dosyası silinir. Süre dosyanın oluşturulmasından değil, başvurunun tamamlanmasından sayılır ve en çok 7 gündür."
	)
	rest := func() string {
		t.Helper()
		return queryString(t, w.d, `SELECT string_agg(code || ' ' || retention::text || ' ' || anchor || ' ' || method, ', ' ORDER BY code)
			FROM retention_policies WHERE code IN ('content_report', 'dsr_export_file')`)
	}
	const wantRest = "content_report 1 year closed_at hard_delete, dsr_export_file 7 days created_at file_delete"
	// 000180 (BE-180) rewrote the content_report text after 000178: down in order.
	exec("000180_snapshot_reported_orgs.down.sql")
	if got, got2 := text("content_report"), text("dsr_export_file"); got != reportNew || got2 != exportNew {
		t.Fatalf("after up: %q, %q", got, got2)
	}
	if got := rest(); got != wantRest {
		t.Fatalf("period/anchor/method = %q, want %q", got, wantRest)
	}

	exec("000178_data_retention_policy_descriptions.down.sql")
	if got, got2 := text("content_report"), text("dsr_export_file"); got != reportOld || got2 != exportOld {
		t.Fatalf("after down: %q, %q", got, got2)
	}
	exec("000178_data_retention_policy_descriptions.up.sql")
	exec("000178_data_retention_policy_descriptions.up.sql") // a second run changes nothing
	if got, got2 := text("content_report"), text("dsr_export_file"); got != reportNew || got2 != exportNew {
		t.Fatalf("after up again: %q, %q", got, got2)
	}
	if got := rest(); got != wantRest {
		t.Fatalf("period/anchor/method = %q, want %q", got, wantRest)
	}

	// A legally approved row is the operator's: neither direction touches it.
	w.exec(`UPDATE retention_policies SET legal_approved = true, approved_ref = 'BE-176 test' WHERE code = 'dsr_export_file'`)
	exec("000178_data_retention_policy_descriptions.down.sql")
	if got, got2 := text("content_report"), text("dsr_export_file"); got != reportOld || got2 != exportNew {
		t.Fatalf("down with an approved dsr_export_file: %q, %q", got, got2)
	}
}
