// Package money is the only place that does arithmetic on money (05 §9.4,
// 06 H-6). Amounts are int64 kuruş (1 TRY = 100 kuruş); rates are integer
// basis points (1 bp = 0.01 %, 10000 bp = 100 %). float, numeric and ad hoc
// "* 100" / "/ 100" / math.Round outside this package are forbidden.
//
// Rounding (06 H-6): every division rounds half away from zero and negative
// amounts mirror positive ones, f(-x) == -f(x). Refunds and reversals can
// therefore be computed from the negated amount and cancel the original
// exactly. VAT and commission are computed per line, never on totals.
//
// Overflow: every operation reports ErrOverflow instead of wrapping. Results
// are limited to [-MaxInt64, MaxInt64]; math.MinInt64 is rejected as an
// input and never produced, so negation is always safe.
//
// The package is a leaf: it imports only the standard library.
package money

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

// Errors returned by this package. Callers wrap them into their own module
// errors; they are never shown to clients as is.
var (
	ErrOverflow = errors.New("money: overflow")
	ErrInvalid  = errors.New("money: invalid argument")
)

// BPScale is the number of basis points in 100 %.
const BPScale = 10000

// ValidBP reports whether bp is a rate between 0 % and 100 % (the
// `*_bp BETWEEN 0 AND 10000` CHECK of 02 §0.4).
func ValidBP(bp int64) bool { return bp >= 0 && bp <= BPScale }

// magnitude returns |x| as uint64. math.MinInt64 is rejected so that every
// accepted value can be negated (H-6 symmetry).
func magnitude(x int64) (uint64, error) {
	switch {
	case x == math.MinInt64:
		return 0, ErrOverflow
	case x < 0:
		return uint64(-x), nil
	default:
		return uint64(x), nil
	}
}

// signed applies the sign to m and checks the int64 range.
func signed(m uint64, negative bool) (int64, error) {
	if m > math.MaxInt64 {
		return 0, ErrOverflow
	}
	if negative {
		return -int64(m), nil
	}
	return int64(m), nil
}

// MulDiv returns x × num / den rounded half away from zero. The product is
// computed in 128 bits, so any result that fits in int64 is exact; den must
// be positive.
func MulDiv(x, num, den int64) (int64, error) {
	if den <= 0 {
		return 0, ErrInvalid
	}
	return mulDiv(x, num, den, true)
}

// mulDiv computes |x|·|num| / den with the sign of x·num, rounding half away
// from zero (round=true) or toward zero (round=false).
func mulDiv(x, num, den int64, round bool) (int64, error) {
	mx, err := magnitude(x)
	if err != nil {
		return 0, err
	}
	mn, err := magnitude(num)
	if err != nil {
		return 0, err
	}
	d := uint64(den) //nolint:gosec // den > 0: checked by every caller
	hi, lo := bits.Mul64(mx, mn)
	if hi >= d { // the quotient would not fit in 64 bits
		return 0, ErrOverflow
	}
	q, r := bits.Div64(hi, lo, d)
	if round && r >= d-r { // r/d >= 1/2, written without overflow
		q++
		if q == 0 {
			return 0, ErrOverflow
		}
	}
	return signed(q, (x < 0) != (num < 0) && q != 0)
}

// ApplyBP returns amount × bp / 10000 rounded half away from zero:
// 335 kuruş × 10 % = 34 and -335 × 10 % = -34 (0.5 kuruş rounds to 1).
// bp is not range checked here: callers validate stored rates with ValidBP.
func ApplyBP(amount, bp int64) (int64, error) { return mulDiv(amount, bp, BPScale, true) }

// VAT returns the VAT of one line: net × rateBP / 10000, rounded half away
// from zero (02 order_lines calculation rules, with the H-6 rounding).
// rateBP must be in [0, 10000].
func VAT(net, rateBP int64) (int64, error) {
	if !ValidBP(rateBP) {
		return 0, ErrInvalid
	}
	return ApplyBP(net, rateBP)
}

// Commission returns basis × rateBP / 10000 rounded half away from zero and
// then limited by magnitude to [minAmount, maxAmount] (nil = no bound), so a
// refund's commission mirrors the original line's. Bounds are non-negative
// kuruş amounts; rateBP must be in [0, 10000].
func Commission(basis, rateBP int64, minAmount, maxAmount *int64) (int64, error) {
	if !ValidBP(rateBP) ||
		minAmount != nil && *minAmount < 0 || maxAmount != nil && *maxAmount < 0 ||
		minAmount != nil && maxAmount != nil && *minAmount > *maxAmount {
		return 0, ErrInvalid
	}
	c, err := ApplyBP(basis, rateBP)
	if err != nil {
		return 0, err
	}
	m := c
	if m < 0 {
		m = -m
	}
	if minAmount != nil && m < *minAmount && basis != 0 {
		m = *minAmount
	}
	if maxAmount != nil && m > *maxAmount {
		m = *maxAmount
	}
	if basis < 0 {
		return -m, nil
	}
	return m, nil
}

// VATWithheld returns the withheld part of a line's VAT (tevkifat):
// vat × num / den rounded half away from zero, with 0 < num <= den
// (e.g. 5/10). 02 rounds withholding down; H-6 overrides it so that refunds
// mirror (open question T-99: confirm with the accountant).
func VATWithheld(vat, num, den int64) (int64, error) {
	if num <= 0 || den <= 0 || num > den {
		return 0, ErrInvalid
	}
	return mulDiv(vat, num, den, true)
}

// Add returns a + b.
func Add(a, b int64) (int64, error) { return Sum(a, b) }

// Sub returns a - b.
func Sub(a, b int64) (int64, error) {
	if b == math.MinInt64 {
		return 0, ErrOverflow
	}
	return Sum(a, -b)
}

// Mul returns amount × qty (e.g. unit price × quantity), exactly.
func Mul(amount, qty int64) (int64, error) { return mulDiv(amount, qty, 1, false) }

// Sum returns the total of xs.
func Sum(xs ...int64) (int64, error) {
	var s int64
	for _, x := range xs {
		if x == math.MinInt64 {
			return 0, ErrOverflow
		}
		if x > 0 && s > math.MaxInt64-x || x < 0 && s < -math.MaxInt64-x {
			return 0, ErrOverflow
		}
		s += x
	}
	return s, nil
}

// Allocate splits total in proportion to weights without losing a kuruş:
// sum(parts) == total. Each part gets floor(|total| × w / Σw) and the
// remaining kuruş go one by one to the largest remainders (ties: lower
// index first). A negative total mirrors the positive split. Weights must be
// non-negative with a positive sum.
func Allocate(total int64, weights []int64) ([]int64, error) {
	if len(weights) == 0 {
		return nil, ErrInvalid
	}
	var sumW int64
	for _, w := range weights {
		if w < 0 {
			return nil, ErrInvalid
		}
		if sumW > math.MaxInt64-w {
			return nil, ErrOverflow
		}
		sumW += w
	}
	if sumW == 0 {
		return nil, ErrInvalid
	}
	mt, err := magnitude(total)
	if err != nil {
		return nil, err
	}
	parts := make([]int64, len(weights))
	rems := make([]uint64, len(weights))
	var given uint64
	for i, w := range weights {
		hi, lo := bits.Mul64(mt, uint64(w))      //nolint:gosec // w >= 0 checked above
		q, r := bits.Div64(hi, lo, uint64(sumW)) // hi < sumW because w <= sumW
		parts[i], rems[i] = int64(q), r          //nolint:gosec // q <= |total| <= MaxInt64
		given += q
	}
	for left := mt - given; left > 0; left-- {
		best := -1
		for i, r := range rems {
			if weights[i] > 0 && (best < 0 || r > rems[best]) {
				best = i
			}
		}
		parts[best]++
		rems[best] = 0
	}
	if total < 0 {
		for i := range parts {
			parts[i] = -parts[i]
		}
	}
	return parts, nil
}

// Decimal formats amount for external systems that take a decimal TRY
// amount (Paraşüt `number`, iyzico `decimal`): 12345 → "123.45",
// -5 → "-0.05". Kuruş leave the system only through this function.
func Decimal(amount int64) string {
	sign, m := "", uint64(0)
	if amount < 0 {
		sign, m = "-", uint64(-amount) // two's complement: also correct for math.MinInt64
	} else {
		m = uint64(amount)
	}
	return fmt.Sprintf("%s%d.%02d", sign, m/100, m%100)
}

// ParseDecimal parses a decimal TRY amount from an external system
// ("123.45", "-0.5", "7") into kuruş. At most two fraction digits are
// accepted (no rounding); signs other than a leading "-", exponents,
// thousands separators and whitespace are rejected.
func ParseDecimal(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	intPart, frac, hasDot := strings.Cut(body, ".")
	if intPart == "" || !allDigits(intPart) || hasDot && (frac == "" || len(frac) > 2 || !allDigits(frac)) {
		return 0, fmt.Errorf("%w: malformed decimal amount", ErrInvalid)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	whole, err := strconv.ParseUint(intPart, 10, 64)
	if err != nil {
		return 0, ErrOverflow
	}
	cents, _ := strconv.ParseUint(frac, 10, 64) // two digits, cannot fail
	hi, lo := bits.Mul64(whole, 100)
	m, carry := bits.Add64(lo, cents, 0)
	if hi != 0 || carry != 0 {
		return 0, ErrOverflow
	}
	return signed(m, neg && m != 0)
}

func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
