package sms_test

import (
	"errors"
	"testing"

	"koro/internal/platform/sms"
)

// TestSMS_NormalizeNumber pins the Turkish mobile number rule (E.164,
// +905XXXXXXXXX) on the written forms users type.
func TestSMS_NormalizeNumber(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"+905550000001", "+90 555 000 00 01", "+90 (555) 000-00-01", "0090 555 000 00 01", "905550000001",
		"0555 000 00 01", "05550000001", "555 000 00 01", "5550000001", " 555.000.00.01 ",
	} {
		got, err := sms.NormalizeNumber(in)
		if err != nil || got != "+905550000001" {
			t.Errorf("NormalizeNumber(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{
		"", "+90 212 000 00 01", "0212 000 00 01", "+49 151 0000 0001", "555 000 00 0", "555 000 00 011",
		"5550000001x", "+90 555 000 00 01 ext", "++905550000001", "90+5550000001", "٥٥٥٠٠٠٠٠٠١",
	} {
		got, err := sms.NormalizeNumber(in)
		if !errors.Is(err, sms.ErrInvalidNumber) || !errors.Is(err, sms.ErrRejected) || got != "" {
			t.Errorf("NormalizeNumber(%q) = %q, %v; want ErrInvalidNumber", in, got, err)
		}
	}
	if n := sms.National("+905550000001"); n != "5550000001" {
		t.Errorf("National = %q", n)
	}
	if n := sms.National("05550000001"); n != "" {
		t.Errorf("National(not E.164) = %q", n)
	}
}

// TestSMS_MessageNeverPrintsNumberOrText: %v and %#v of a message show the
// reference only.
func TestSMS_MessageNeverPrintsNumberOrText(t *testing.T) {
	t.Parallel()
	m := sms.Message{To: "+905550000001", Text: "kod 482915", Ref: "r1"}
	for _, s := range []string{m.String(), m.GoString()} {
		if s != "sms.Message{ref:r1}" {
			t.Errorf("printed %q", s)
		}
	}
}
