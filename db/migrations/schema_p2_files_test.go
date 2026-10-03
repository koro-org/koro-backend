package migrations_test

import (
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"

	"koro/db/migrations"
)

// p2Files is the file order of 000094–000120 in 02 §23 (BE-034, BE-001 R2).
// Like p1aFiles, p1bFiles and p1cFiles, the names are a contract.
var p2Files = []string{
	"000094_create_legal_parties",
	"000095_create_legal_party_documents",
	"000096_create_payees",
	"000097_create_payee_bank_accounts",
	"000098_create_services",
	"000099_create_offerings",
	"000100_create_commerce_policies",
	"000101_create_orders",
	"000102_create_order_lines",
	"000103_create_collection_accounts",
	"000104_add_fk_orders_collection_account",
	"000105_create_incoming_bank_transfers",
	"000106_create_payments",
	"000107_create_payment_splits",
	"000108_create_refunds",
	"000109_create_ledger_accounts",
	"000110_seed_ledger_accounts",
	"000111_create_ledger_transactions",
	"000112_create_ledger_entries",
	"000113_create_payouts",
	"000114_add_fk_order_lines_payout",
	"000115_create_payout_items",
	"000116_create_invoices",
	"000117_create_invoice_lines",
	"000118_create_provider_contact_map",
	"000119_create_provider_product_map",
	"000120_create_integration_credentials",
}

// TestMigrations_P2SchemaFileNames: numbers 000094–000120 exist exactly with
// the names of 02 §23, each as an up/down pair starting with its name
// comment, and every up file states the lock timeout of its transaction
// (05 §7.2).
func TestMigrations_P2SchemaFileNames(t *testing.T) {
	t.Parallel()
	var got []string
	for n := 94; n <= 120; n++ {
		prefix := strconv.Itoa(n)
		prefix = strings.Repeat("0", 6-len(prefix)) + prefix
		ups, err := fs.Glob(migrations.FS, prefix+"_*.up.sql")
		if err != nil {
			t.Fatal(err)
		}
		downs, err := fs.Glob(migrations.FS, prefix+"_*.down.sql")
		if err != nil {
			t.Fatal(err)
		}
		if len(ups) != 1 || len(downs) != 1 {
			t.Errorf("migration %s: %d up and %d down files, want one pair", prefix, len(ups), len(downs))
			continue
		}
		name := strings.TrimSuffix(ups[0], ".up.sql")
		if strings.TrimSuffix(downs[0], ".down.sql") != name {
			t.Errorf("migration %s: up is %q but down is %q", prefix, ups[0], downs[0])
		}
		got = append(got, name)
		for _, file := range []string{ups[0], downs[0]} {
			src, err := fs.ReadFile(migrations.FS, file)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(src), "-- "+strings.TrimSuffix(file, ".sql")) {
				t.Errorf("%s: the first line must be the comment \"-- %s…\"", file, strings.TrimSuffix(file, ".sql"))
			}
		}
		up, _ := fs.ReadFile(migrations.FS, ups[0])
		if !strings.Contains(string(up), "SET LOCAL lock_timeout = '5s';") {
			t.Errorf("%s: missing SET LOCAL lock_timeout = '5s'", ups[0])
		}
	}
	if !slices.Equal(got, p2Files) {
		t.Errorf("migration names 000094–000120 differ from 02 §23:\n got %v\nwant %v", got, p2Files)
	}
}

// TestMigrations_P2MoneyColumns (02 §0.4, 06 H-6): every money column of
// the P2 migrations is a bigint in kuruş named *_amount (never numeric or
// float) and every rate is an integer in basis points named *_bp with the
// 0..10000 range; amounts carry a sign CHECK.
func TestMigrations_P2MoneyColumns(t *testing.T) {
	t.Parallel()
	for _, name := range p2Files {
		src, err := fs.ReadFile(migrations.FS, name+".up.sql")
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(sqlLineComment.ReplaceAllString(string(src), ""), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			column, typ := fields[0], strings.ToLower(fields[1])
			if typ == "numeric" || typ == "real" || typ == "float" || strings.HasPrefix(typ, "double") || strings.HasPrefix(typ, "numeric(") || typ == "money" {
				t.Errorf("%s:%d: %s %s: money is bigint kuruş (02 §0.4)", name, i+1, column, typ)
			}
			switch {
			case strings.HasSuffix(column, "_amount"):
				if typ != "bigint" {
					t.Errorf("%s:%d: %s is %s, want bigint (kuruş)", name, i+1, column, typ)
				}
				if !strings.Contains(line, "CHECK ("+column+" >= 0)") && !strings.Contains(line, "CHECK ("+column+" > 0)") && !strings.Contains(line, "CHECK ("+column+" <> 0)") {
					t.Errorf("%s:%d: %s has no sign CHECK", name, i+1, column)
				}
			case strings.HasSuffix(column, "_bp"):
				if typ != "integer" || !strings.Contains(line, "CHECK ("+column+" BETWEEN 0 AND 10000)") {
					t.Errorf("%s:%d: %s must be integer CHECK (BETWEEN 0 AND 10000)", name, i+1, column)
				}
			}
		}
	}
}
