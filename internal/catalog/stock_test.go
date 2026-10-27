package catalog_test

import (
	"errors"
	"testing"

	"koro/internal/catalog"
)

// TestStock_Refusal (BE-139): the one stock rule Reserve (under the row
// lock) and orders' quoteOrder (on the checkout read) apply — fewer units
// left than asked is catalog.sold_out, the buyer's held units plus the
// quantity over per_buyer_limit is catalog.per_buyer_limit; the held count
// is read only for an offering with a limit, and its error comes back.
func TestStock_Refusal(t *testing.T) {
	t.Parallel()
	n := func(v int) *int { return &v }
	readErr := errors.New("held read failed")
	cases := []struct {
		name  string
		stock catalog.Stock
		qty   int
		held  int
		err   error // held's error
		want  error
		reads bool
	}{
		{"unlimited, no limit", catalog.Stock{}, 1000, 0, nil, nil, false},
		{"exactly the units left", catalog.Stock{Remaining: n(3)}, 3, 0, nil, nil, false},
		{"more than left", catalog.Stock{Remaining: n(2)}, 3, 0, nil, catalog.ErrSoldOut, false},
		{"sold out before the limit", catalog.Stock{Remaining: n(0), PerBuyerLimit: n(1)}, 1, 5, nil, catalog.ErrSoldOut, false},
		{"within the limit", catalog.Stock{PerBuyerLimit: n(3)}, 1, 2, nil, nil, true},
		{"over the limit", catalog.Stock{Remaining: n(10), PerBuyerLimit: n(3)}, 2, 2, nil, catalog.ErrPerBuyerLimit, true},
		{"held read fails", catalog.Stock{PerBuyerLimit: n(3)}, 1, 0, readErr, readErr, true},
	}
	for _, tc := range cases {
		read := false
		got := tc.stock.Refusal(tc.qty, func() (int, error) { read = true; return tc.held, tc.err })
		if !errors.Is(got, tc.want) || (tc.want == nil) != (got == nil) || read != tc.reads {
			t.Errorf("%s: Refusal = %v (held read %v), want %v (read %v)", tc.name, got, read, tc.want, tc.reads)
		}
	}
	if err := (catalog.Stock{PerBuyerLimit: n(1)}).Refusal(1, nil); err != nil {
		t.Errorf("no held count is 0: %v", err)
	}
}
