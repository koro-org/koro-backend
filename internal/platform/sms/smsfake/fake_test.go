package smsfake_test

import (
	"errors"
	"fmt"
	"testing"

	"koro/internal/platform/sms"
	"koro/internal/platform/sms/smscontract"
	"koro/internal/platform/sms/smsfake"
)

// TestSMSFake_Contract: the in-memory adapter passes the SMS contract.
func TestSMSFake_Contract(t *testing.T) {
	t.Parallel()
	smscontract.Run(t, func(*testing.T) smscontract.Harness {
		f := smsfake.New()
		return smscontract.Harness{
			Adapter:   f,
			Delivered: f.Sent,
			Next: func(o smscontract.Outcome) {
				switch o {
				case smscontract.BadNumber:
					f.FailNext(fmt.Errorf("smsfake: %w: %w", sms.ErrRejected, sms.ErrInvalidNumber))
				case smscontract.Throttled:
					f.FailNext(fmt.Errorf("smsfake: %w", sms.ErrThrottled))
				case smscontract.Refused:
					f.FailNext(fmt.Errorf("smsfake: %w", sms.ErrRejected))
				case smscontract.Down:
					f.FailNext(errors.New("smsfake: provider down"))
				}
			},
		}
	})
}

// TestSMSFake_LastAndPrint: Last finds the newest message of a number;
// printing the fake never shows a message.
func TestSMSFake_LastAndPrint(t *testing.T) {
	t.Parallel()
	f := smsfake.New()
	for _, text := range []string{"kod 111111", "kod 222222"} {
		if _, err := f.Send(t.Context(), sms.Message{To: smscontract.Number, Text: text, Ref: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	if m, ok := f.Last(smscontract.Number); !ok || m.Text != "kod 222222" {
		t.Fatalf("Last = %v, %v", m, ok)
	}
	if _, ok := f.Last("+905550000099"); ok {
		t.Fatal("Last of an unknown number")
	}
	if s := fmt.Sprint(f); s != "smsfake.Fake{2 sent}" {
		t.Fatalf("printed %q", s)
	}
}
