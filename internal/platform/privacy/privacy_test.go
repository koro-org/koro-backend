package privacy_test

import (
	"testing"
	"uuid"

	"koro/internal/platform/privacy"
)

func TestSubject_RoundTripsThroughContext(t *testing.T) {
	t.Parallel()
	if _, ok := privacy.SubjectFrom(t.Context()); ok {
		t.Fatal("empty context must not carry a subject")
	}
	want := privacy.Subject{UserID: uuid.NewV7(), DSRID: uuid.NewV7()}
	got, ok := privacy.SubjectFrom(privacy.WithSubject(t.Context(), want))
	if !ok || got != want {
		t.Fatalf("SubjectFrom() = %+v, %v; want %+v", got, ok, want)
	}
}
