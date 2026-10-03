package rewrap_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"koro/internal/platform/clock"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
	"koro/internal/platform/queue"
)

type nopEnvelope struct{}

func (nopEnvelope) ActiveKeyID(context.Context) (string, error) { return "k1", nil }
func (nopEnvelope) Rewrap(_ context.Context, _ string, b []byte) ([]byte, bool, error) {
	return b, false, nil
}

func TestRewrap_NewValidatesColumns(t *testing.T) {
	t.Parallel()
	deps := func(cols ...rewrap.Column) rewrap.Deps {
		return rewrap.Deps{ORM: &gorm.DB{}, Tx: db.NopTx{}, Envelope: nopEnvelope{}, Columns: cols,
			Clock: clock.NewFake(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)), Log: obs.Discard()}
	}
	good := rewrap.Column{Name: "identity.mfa_totp.secret_enc", Table: "mfa_totp", Key: "user_id", Column: "secret_enc",
		PurposePrefix: "identity.mfa_secret/", PurposeColumn: "user_id"}
	j, err := rewrap.New(deps(good))
	if err != nil {
		t.Fatal(err)
	}
	qj := j.QueueJob()
	if qj.Name != rewrap.JobName || qj.Queue != queue.Low || qj.Validate() != nil {
		t.Fatalf("job = %+v (%v)", qj, qj.Validate())
	}
	bad := []rewrap.Column{
		{Name: "x", Table: "t; DROP TABLE users", Key: "id", Column: "c", PurposePrefix: "p"},
		{Name: "x", Table: "t", Key: "id\"", Column: "c", PurposePrefix: "p"},
		{Name: "x", Table: "t", Key: "id", Column: "c", PurposePrefix: "p", KeyType: "uuid) OR (1=1"},
		{Name: "x", Table: "t", Key: "id", Column: "c"},
		{Table: "t", Key: "id", Column: "c", PurposePrefix: "p"},
	}
	for _, c := range bad {
		if _, err := rewrap.New(deps(c)); err == nil {
			t.Errorf("New accepted %+v", c)
		}
	}
	if _, err := rewrap.New(deps(good, good)); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate column = %v", err)
	}
	if _, err := rewrap.New(rewrap.Deps{}); err == nil {
		t.Fatal("New without dependencies succeeded")
	}
}
