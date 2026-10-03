package money_test

import (
	"errors"
	"math"
	"math/big"
	"testing"

	"koro/internal/platform/money"
)

const maxI = math.MaxInt64

func ptr(v int64) *int64 { return &v }

// refMulDiv is an independent big-integer reference for x × num / den
// rounded half away from zero: sign(x·num) · floor((2|x·num| + den) / 2den).
func refMulDiv(x, num, den int64) (int64, bool) {
	p := new(big.Int).Mul(big.NewInt(x), big.NewInt(num))
	neg := p.Sign() < 0
	p.Abs(p)
	d := big.NewInt(den)
	q := new(big.Int).Lsh(p, 1)
	q.Add(q, d)
	q.Quo(q, new(big.Int).Lsh(d, 1))
	if neg {
		q.Neg(q)
	}
	if !q.IsInt64() || q.Int64() == math.MinInt64 {
		return 0, false
	}
	return q.Int64(), true
}

func TestMoney_RoundHalfAwayFromZero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		amount, bp int64
		want       int64
	}{
		{"+0.5 kuruş rounds up", 1, 5000, 1},
		{"-0.5 kuruş rounds down", -1, 5000, -1},
		{"+0.4999 kuruş rounds to zero", 1, 4999, 0},
		{"-0.4999 kuruş rounds to zero", -1, 4999, 0},
		{"+0.5001 kuruş", 1, 5001, 1},
		{"+1.5 → 2", 3, 5000, 2},
		{"-1.5 → -2", -3, 5000, -2},
		{"+2.5 → 3 (not banker's 2)", 5, 5000, 3},
		{"-2.5 → -3 (not banker's -2)", -5, 5000, -3},
		{"05 §9.4 example +33.5", 335, 1000, 34},
		{"05 §9.4 example -33.5 (02 formula gives -33)", -335, 1000, -34},
		{"+0.0001 kuruş", 1, 1, 0},
		{"zero", 0, 5000, 0},
		{"max int64 × 100 % is exact", maxI, 10000, maxI},
		{"-max int64 × 100 % is exact", -maxI, 10000, -maxI},
		{"max int64 × 50 % = x.5 rounds away", maxI, 5000, 4611686018427387904},
		{"-max int64 × 50 % mirrors", -maxI, 5000, -4611686018427387904},
		{"max int64 × 1 bp", maxI, 1, 922337203685478},
		{"max int64 × 9999 bp", maxI, 9999, 9222449699651090329},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := money.ApplyBP(tt.amount, tt.bp)
			if err != nil || got != tt.want {
				t.Fatalf("ApplyBP(%d, %d) = %d, %v; want %d", tt.amount, tt.bp, got, err, tt.want)
			}
			if ref, ok := refMulDiv(tt.amount, tt.bp, money.BPScale); !ok || ref != tt.want {
				t.Fatalf("reference disagrees with the table: %d, %v", ref, ok)
			}
		})
	}
}

func TestMoney_NegativeMirrorsPositive(t *testing.T) {
	t.Parallel()
	amounts := []int64{0, 1, 2, 3, 5, 49, 50, 51, 99, 101, 335, 12345, 99999, 1_000_000, 123_456_789, maxI / 3, maxI / 2, maxI - 1, maxI}
	bps := []int64{0, 1, 18, 50, 99, 100, 800, 1000, 1800, 2000, 3333, 4999, 5000, 5001, 9999, 10000}
	nums := [][2]int64{{1, 2}, {2, 10}, {5, 10}, {7, 10}, {9, 10}, {1, 3}, {2, 3}, {10, 10}}
	for _, a := range amounts {
		for _, bp := range bps {
			pos, perr := money.ApplyBP(a, bp)
			neg, nerr := money.ApplyBP(-a, bp)
			if (perr == nil) != (nerr == nil) || pos != -neg {
				t.Errorf("ApplyBP(±%d, %d) = %d (%v) / %d (%v); want mirrored", a, bp, pos, perr, neg, nerr)
			}
			pc, perr := money.Commission(a, bp, ptr(7), ptr(1_000_000))
			nc, nerr := money.Commission(-a, bp, ptr(7), ptr(1_000_000))
			if (perr == nil) != (nerr == nil) || pc != -nc {
				t.Errorf("Commission(±%d, %d) = %d / %d; want mirrored", a, bp, pc, nc)
			}
		}
		for _, f := range nums {
			pos, perr := money.VATWithheld(a, f[0], f[1])
			neg, nerr := money.VATWithheld(-a, f[0], f[1])
			if (perr == nil) != (nerr == nil) || pos != -neg {
				t.Errorf("VATWithheld(±%d, %d/%d) = %d / %d; want mirrored", a, f[0], f[1], pos, neg)
			}
		}
		total := []int64{1, 1, 1}
		pos, _ := money.Allocate(a, total)
		neg, _ := money.Allocate(-a, total)
		for i := range pos {
			if pos[i] != -neg[i] {
				t.Errorf("Allocate(±%d) = %v / %v; want mirrored", a, pos, neg)
				break
			}
		}
	}
}

func TestMoney_ApplyBP_Table(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		amount, bp int64
		want       int64
	}{
		{"VAT 20 % on 1.000.000 (02 example)", 1_000_000, 2000, 200_000},
		{"commission 10 % on 1.000.000 (02 example)", 1_000_000, 1000, 100_000},
		{"commission VAT 20 % on 100.000", 100_000, 2000, 20_000},
		{"VAT 20 % on 1 kuruş = 0.2", 1, 2000, 0},
		{"VAT 20 % on 3 kuruş = 0.6", 3, 2000, 1},
		{"VAT 20 % on 99,99 TL", 9999, 2000, 2000},
		{"VAT 10 % on 99,95 TL = 9.995", 9995, 1000, 1000},
		{"VAT 1 % on 149,50 TL = 1.495", 14950, 100, 150},
		{"VAT 8 % on 12,34 TL = 0.9872", 1234, 800, 99},
		{"0 %", 123_456, 0, 0},
		{"100 %", 123_456, 10000, 123_456},
		{"refund VAT 20 % on -3 kuruş", -3, 2000, -1},
		{"refund commission 10 % on -1.000.000", -1_000_000, 1000, -100_000},
		{"18 % on 2,75 TL = 0.495", 275, 1800, 50},
		{"18 % on -2,75 TL", -275, 1800, -50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := money.ApplyBP(tt.amount, tt.bp)
			if err != nil || got != tt.want {
				t.Fatalf("ApplyBP(%d, %d) = %d, %v; want %d", tt.amount, tt.bp, got, err, tt.want)
			}
		})
	}

	t.Run("VAT validates the rate", func(t *testing.T) {
		t.Parallel()
		if v, err := money.VAT(1_000_000, 2000); err != nil || v != 200_000 {
			t.Fatalf("VAT = %d, %v", v, err)
		}
		for _, bp := range []int64{-1, 10001} {
			if _, err := money.VAT(100, bp); !errors.Is(err, money.ErrInvalid) {
				t.Fatalf("VAT(100, %d) error = %v, want ErrInvalid", bp, err)
			}
		}
	})

	t.Run("02 worked example (model A, seller pays 10 %)", func(t *testing.T) {
		t.Parallel()
		net, _ := money.Mul(1_000_000, 1)
		vat, _ := money.VAT(net, 2000)
		gross, _ := money.Add(net, vat)
		comm, _ := money.Commission(net, 1000, nil, nil)
		commVAT, _ := money.VAT(comm, 2000)
		sellerNet, err := money.Sum(gross, -comm, -commVAT)
		if err != nil || vat != 200_000 || gross != 1_200_000 || comm != 100_000 || commVAT != 20_000 || sellerNet != 1_080_000 {
			t.Fatalf("vat=%d gross=%d comm=%d commVAT=%d sellerNet=%d err=%v", vat, gross, comm, commVAT, sellerNet, err)
		}
	})
}

func TestMoney_Commission_Bounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		basis, bp      int64
		minAmt, maxAmt *int64
		want           int64
		wantErr        error
	}{
		{"no bounds", 10_000, 1000, nil, nil, 1000, nil},
		{"below min", 10_000, 1000, ptr(1500), nil, 1500, nil},
		{"above max", 10_000, 1000, nil, ptr(800), 800, nil},
		{"inside", 10_000, 1000, ptr(500), ptr(2000), 1000, nil},
		{"refund below min mirrors", -10_000, 1000, ptr(1500), nil, -1500, nil},
		{"refund above max mirrors", -10_000, 1000, nil, ptr(800), -800, nil},
		{"zero basis has no minimum", 0, 1000, ptr(1500), nil, 0, nil},
		{"rate over 100 %", 10_000, 10001, nil, nil, 0, money.ErrInvalid},
		{"negative min", 10_000, 1000, ptr(-1), nil, 0, money.ErrInvalid},
		{"min > max", 10_000, 1000, ptr(10), ptr(5), 0, money.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := money.Commission(tt.basis, tt.bp, tt.minAmt, tt.maxAmt)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("Commission = %d, %v; want %d, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestMoney_VATWithheld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		vat, num, den int64
		want          int64
		wantErr       error
	}{
		{"5/10 of 200.000", 200_000, 5, 10, 100_000, nil},
		{"9/10 of 15 = 13.5", 15, 9, 10, 14, nil},
		{"2/10 of 3 = 0.6", 3, 2, 10, 1, nil},
		{"2/10 of 2 = 0.4", 2, 2, 10, 0, nil},
		{"refund 9/10 of -15", -15, 9, 10, -14, nil},
		{"full 10/10", 777, 10, 10, 777, nil},
		{"num > den", 100, 11, 10, 0, money.ErrInvalid},
		{"zero num", 100, 0, 10, 0, money.ErrInvalid},
		{"zero den", 100, 1, 0, 0, money.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := money.VATWithheld(tt.vat, tt.num, tt.den)
			if !errors.Is(err, tt.wantErr) || got != tt.want {
				t.Fatalf("VATWithheld = %d, %v; want %d, %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestMoney_Overflow_ReturnsError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		f    func() (int64, error)
	}{
		{"ApplyBP above 100 % of max", func() (int64, error) { return money.ApplyBP(maxI, 10001) }},
		{"ApplyBP -max above 100 %", func() (int64, error) { return money.ApplyBP(-maxI, 10001) }},
		{"ApplyBP min int64 input", func() (int64, error) { return money.ApplyBP(math.MinInt64, 1) }},
		{"ApplyBP min int64 bp", func() (int64, error) { return money.ApplyBP(1, math.MinInt64) }},
		{"ApplyBP 128-bit quotient", func() (int64, error) { return money.ApplyBP(maxI, maxI) }},
		// 65535 × 281479271743489 = 2^64-1, so the quotient is 2^63 - 0.5 and rounds to 2^63.
		{"MulDiv rounds past max", func() (int64, error) { return money.MulDiv(65535, 281479271743489, 2) }},
		{"Mul", func() (int64, error) { return money.Mul(maxI/2+1, 2) }},
		{"Mul negative", func() (int64, error) { return money.Mul(-(maxI/2 + 1), 2) }},
		{"Add", func() (int64, error) { return money.Add(maxI, 1) }},
		{"Add negative reaches min int64", func() (int64, error) { return money.Add(-maxI, -1) }},
		{"Sub", func() (int64, error) { return money.Sub(-maxI, 1) }},
		{"Sub min int64", func() (int64, error) { return money.Sub(0, math.MinInt64) }},
		{"Sum", func() (int64, error) { return money.Sum(maxI/2, maxI/2, 2) }},
		{"Sum min int64 element", func() (int64, error) { return money.Sum(math.MinInt64) }},
		{"ParseDecimal", func() (int64, error) { return money.ParseDecimal("92233720368547758.08") }},
		{"ParseDecimal huge", func() (int64, error) { return money.ParseDecimal("99999999999999999999999") }},
		{"Commission", func() (int64, error) { return money.Commission(math.MinInt64, 1, nil, nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tt.f()
			if !errors.Is(err, money.ErrOverflow) || got != 0 {
				t.Fatalf("got %d, %v; want 0, ErrOverflow", got, err)
			}
		})
	}

	t.Run("boundaries just inside", func(t *testing.T) {
		t.Parallel()
		for name, f := range map[string]func() (int64, error){
			"Add to max":          func() (int64, error) { return money.Add(maxI-1, 1) },
			"Add to -max":         func() (int64, error) { return money.Add(-maxI+1, -1) },
			"Mul to max":          func() (int64, error) { return money.Mul(maxI, 1) },
			"MulDiv 128-bit path": func() (int64, error) { return money.MulDiv(maxI, maxI, maxI) },
			"ParseDecimal max":    func() (int64, error) { return money.ParseDecimal("92233720368547758.07") },
		} {
			if _, err := f(); err != nil {
				t.Errorf("%s: unexpected error %v", name, err)
			}
		}
	})

	t.Run("allocate", func(t *testing.T) {
		t.Parallel()
		if _, err := money.Allocate(1, []int64{maxI, 1}); !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("Allocate weight sum overflow error = %v", err)
		}
		if _, err := money.Allocate(math.MinInt64, []int64{1}); !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("Allocate(min int64) error = %v", err)
		}
	})
}

func TestMoney_Allocate_PreservesTotal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		total   int64
		weights []int64
		want    []int64
	}{
		{"three ways", 100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{"negative three ways mirrors", -100, []int64{1, 1, 1}, []int64{-34, -33, -33}},
		{"largest remainder wins", 10, []int64{1, 2, 3}, []int64{2, 3, 5}},
		{"zero weight gets nothing", 7, []int64{0, 1, 1}, []int64{0, 4, 3}},
		{"single", 12345, []int64{9}, []int64{12345}},
		{"zero total", 0, []int64{1, 2}, []int64{0, 0}},
		{"huge total", maxI, []int64{1, 1}, []int64{maxI/2 + 1, maxI / 2}},
		{"huge weights", 1000, []int64{maxI / 2, maxI / 2}, []int64{500, 500}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := money.Allocate(tt.total, tt.weights)
			if err != nil {
				t.Fatalf("Allocate error = %v", err)
			}
			sum, _ := money.Sum(got...)
			if sum != tt.total {
				t.Fatalf("sum(parts) = %d, want %d", sum, tt.total)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Allocate = %v, want %v", got, tt.want)
				}
			}
		})
	}
	for _, w := range [][]int64{nil, {}, {0, 0}, {1, -1}} {
		if _, err := money.Allocate(10, w); !errors.Is(err, money.ErrInvalid) {
			t.Errorf("Allocate(10, %v) error = %v, want ErrInvalid", w, err)
		}
	}
}

func TestMoney_DecimalAndParse(t *testing.T) {
	t.Parallel()
	formats := []struct {
		amount int64
		want   string
	}{
		{0, "0.00"}, {5, "0.05"}, {-5, "-0.05"}, {100, "1.00"}, {12345, "123.45"}, {-12345, "-123.45"},
		{maxI, "92233720368547758.07"}, {math.MinInt64, "-92233720368547758.08"},
	}
	for _, f := range formats {
		if got := money.Decimal(f.amount); got != f.want {
			t.Errorf("Decimal(%d) = %q, want %q", f.amount, got, f.want)
		}
	}
	parses := []struct {
		in      string
		want    int64
		wantErr error
	}{
		{"123.45", 12345, nil}, {"-123.45", -12345, nil}, {"7", 700, nil}, {"0.5", 50, nil}, {"-0.05", -5, nil},
		{"-0", 0, nil}, {"007.10", 710, nil},
		{"", 0, money.ErrInvalid}, {"-", 0, money.ErrInvalid}, {"1.", 0, money.ErrInvalid}, {".5", 0, money.ErrInvalid},
		{"1.234", 0, money.ErrInvalid}, {"+1", 0, money.ErrInvalid}, {"1e3", 0, money.ErrInvalid},
		{"1,50", 0, money.ErrInvalid}, {" 1.00", 0, money.ErrInvalid}, {"1 000", 0, money.ErrInvalid}, {"--1", 0, money.ErrInvalid},
		{"١٢", 0, money.ErrInvalid},
	}
	for _, p := range parses {
		got, err := money.ParseDecimal(p.in)
		if !errors.Is(err, p.wantErr) || got != p.want {
			t.Errorf("ParseDecimal(%q) = %d, %v; want %d, %v", p.in, got, err, p.want, p.wantErr)
		}
	}
}

func FuzzMoney_MulDiv(f *testing.F) {
	for _, s := range [][3]int64{{1, 5000, 10000}, {-335, 1000, 10000}, {maxI, 10000, 10000}, {maxI, 5000, 10000}, {-7, 9, 10}, {math.MinInt64, 1, 2}} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, x, num, den int64) {
		if den <= 0 {
			if _, err := money.MulDiv(x, num, den); !errors.Is(err, money.ErrInvalid) {
				t.Fatalf("MulDiv with den=%d error = %v", den, err)
			}
			return
		}
		got, err := money.MulDiv(x, num, den)
		want, ok := refMulDiv(x, num, den)
		inputOK := x != math.MinInt64 && num != math.MinInt64
		switch {
		case inputOK && ok && (err != nil || got != want):
			t.Fatalf("MulDiv(%d, %d, %d) = %d, %v; want %d", x, num, den, got, err, want)
		case (!inputOK || !ok) && !errors.Is(err, money.ErrOverflow):
			t.Fatalf("MulDiv(%d, %d, %d) = %d, %v; want ErrOverflow", x, num, den, got, err)
		}
		if inputOK {
			neg, nerr := money.MulDiv(-x, num, den)
			if (err == nil) != (nerr == nil) || got != -neg {
				t.Fatalf("MulDiv(±%d, %d, %d) not mirrored: %d / %d", x, num, den, got, neg)
			}
		}
	})
}

func FuzzMoney_DecimalRoundTrip(f *testing.F) {
	for _, s := range []int64{0, 1, -1, 99, 100, -12345, maxI, -maxI} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, amount int64) {
		if amount == math.MinInt64 {
			return
		}
		got, err := money.ParseDecimal(money.Decimal(amount))
		if err != nil || got != amount {
			t.Fatalf("ParseDecimal(Decimal(%d)) = %d, %v", amount, got, err)
		}
	})
}
