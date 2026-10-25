//go:build integration

package notify_test

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"koro/internal/notify"
)

// mailpitMessage is the part of Mailpit's /api/v1/message/{id} that the test reads.
type mailpitMessage struct {
	ID          string
	MessageID   string
	From        struct{ Name, Address string }
	To          []struct{ Name, Address string }
	Subject     string
	Text        string
	HTML        string
	Inline      []mailpitPart
	Attachments []mailpitPart
}

type mailpitPart struct {
	ContentID   string
	ContentType string
	FileName    string
}

// startMailpit runs Mailpit (R-002: v1.28) and returns the SMTP address and
// the base URL of its HTTP API.
func startMailpit(t *testing.T) (host string, smtpPort int, api string) {
	t.Helper()
	ctr, err := testcontainers.Run(t.Context(), "axllent/mailpit:v1.28",
		testcontainers.WithExposedPorts("1025/tcp", "8025/tcp"),
		testcontainers.WithWaitStrategy(wait.ForHTTP("/readyz").WithPort("8025/tcp").WithStartupTimeout(60*time.Second)),
	)
	testcontainers.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("start Mailpit: %v", err)
	}
	smtp, err := ctr.PortEndpoint(t.Context(), "1025/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	web, err := ctr.PortEndpoint(t.Context(), "8025/tcp", "http")
	if err != nil {
		t.Fatal(err)
	}
	h, p, err := net.SplitHostPort(smtp)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return h, port, web
}

func mailpitGet(t *testing.T, url string, into any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// waitForMessages polls Mailpit until n messages arrived and returns them
// oldest first.
func waitForMessages(t *testing.T, api string, n int) []mailpitMessage {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var list struct {
			Messages []struct{ ID string }
		}
		mailpitGet(t, api+"/api/v1/messages", &list)
		if len(list.Messages) >= n {
			out := make([]mailpitMessage, 0, n)
			for i := len(list.Messages) - 1; i >= 0; i-- { // Mailpit lists newest first
				var m mailpitMessage
				mailpitGet(t, api+"/api/v1/message/"+list.Messages[i].ID, &m)
				out = append(out, m)
			}
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("Mailpit received %d messages, want %d", len(list.Messages), n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSMTP_Mailpit_Delivers(t *testing.T) {
	t.Parallel()
	host, port, api := startMailpit(t)

	smtp, err := notify.NewSMTPTransport(notify.SMTPConfig{Host: host, Port: port, TLS: notify.SMTPTLSNone, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("NewSMTPTransport() error = %v", err)
	}
	f := newFixture(t, func(d *notify.Deps) {
		d.Transport = smtp
		d.Consent = &fakeConsent{active: true}
		d.Templates = loadTemplates(t, withPromo(baseTemplates(t)))
	})

	// 1. Informational e-mail: Turkish text, CID QR image, .ics attachment.
	m := messageWithQRAndICS(t, "ticket:r1:v1")
	m.Data["name"] = "Ayşe Yılmaz"
	id := mustSend(t, f, m)
	d := mustGet(t, f.store, id)
	if d.Status != notify.StatusSent || d.Provider != "smtp" || d.ProviderMessageID == "" {
		t.Fatalf("delivery = %+v, want sent via smtp", d)
	}

	// 2. Commercial e-mail.
	mustSend(t, f, commercialMessage("promo:1"))

	// 3. A repeated Send must not put a second mail into the inbox.
	if again := mustSend(t, f, m); again != id {
		t.Fatalf("repeated Send() = %s, want %s", again, id)
	}

	msgs := waitForMessages(t, api, 2)
	time.Sleep(300 * time.Millisecond) // a wrongly sent duplicate would be here by now
	var all struct{ Total int }
	mailpitGet(t, api+"/api/v1/messages", &all)
	if all.Total != 2 {
		t.Fatalf("Mailpit holds %d messages, want 2 (the dedupe key must stop the repeat)", all.Total)
	}

	got := msgs[0]
	if got.From.Address != "noreply@koro.test" || got.From.Name != "Koro" {
		t.Errorf("From = %+v", got.From)
	}
	if len(got.To) != 1 || got.To[0].Address != "ayse.yilmaz@ogrenci.edu.tr" {
		t.Errorf("To = %+v", got.To)
	}
	if got.Subject != "Koro test iletisi" {
		t.Errorf("Subject = %q", got.Subject)
	}
	if !strings.Contains(got.Text, "Merhaba Ayşe Yılmaz") || !strings.Contains(got.HTML, "Merhaba Ayşe Yılmaz") {
		t.Errorf("Turkish characters were mangled:\ntext: %q\nhtml: %q", got.Text, got.HTML)
	}
	if !strings.Contains(got.Text, "ticari elektronik ileti değildir") {
		t.Errorf("transactional footer missing from the text part: %q", got.Text)
	}
	if len(got.Inline) != 1 || got.Inline[0].ContentID != "qr.png" || got.Inline[0].ContentType != "image/png" {
		t.Errorf("Inline = %+v, want the CID image qr.png", got.Inline)
	}
	if !strings.Contains(got.HTML, `cid:qr.png`) {
		t.Errorf("HTML does not reference the CID image: %q", got.HTML)
	}
	if len(got.Attachments) != 1 || got.Attachments[0].FileName != "invite.ics" || !strings.HasPrefix(got.Attachments[0].ContentType, "text/calendar") {
		t.Errorf("Attachments = %+v, want invite.ics", got.Attachments)
	}
	if got.MessageID != d.ProviderMessageID {
		t.Errorf("Message-ID %q does not match the recorded provider message ID %q", got.MessageID, d.ProviderMessageID)
	}

	var h map[string][]string
	mailpitGet(t, api+"/api/v1/message/"+got.ID+"/headers", &h)
	if len(h["List-Unsubscribe"]) != 0 || len(h["List-Unsubscribe-Post"]) != 0 {
		t.Errorf("informational mail carries List-Unsubscribe: %v", h)
	}

	promo := msgs[1]
	mailpitGet(t, api+"/api/v1/message/"+promo.ID+"/headers", &h)
	if v := h["List-Unsubscribe"]; len(v) != 1 || v[0] != "<https://koro.test/unsubscribe/abc>" {
		t.Errorf("commercial List-Unsubscribe = %v", v)
	}
	if v := h["List-Unsubscribe-Post"]; len(v) != 1 || v[0] != "List-Unsubscribe=One-Click" {
		t.Errorf("commercial List-Unsubscribe-Post = %v", v)
	}
	if !strings.Contains(promo.Text, "ticari elektronik iletidir") {
		t.Errorf("commercial footer missing: %q", promo.Text)
	}
}

// TestSMTPTransport_ContractSuite_Mailpit runs the EmailTransport contract
// suite (BE-113) against Mailpit, the relay of `make dev`. Mailpit accepts
// every recipient, so the refusal steps run against the fake server only.
func TestSMTPTransport_ContractSuite_Mailpit(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	host, port, api := startMailpit(t)
	tr, err := notify.NewSMTPTransport(notify.SMTPConfig{Host: host, Port: port, TLS: notify.SMTPTLSNone, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	emailTransportContract(t, tr, emailFixture{Delivered: func(t *testing.T) []string {
		var list struct {
			Messages []struct{ MessageID string }
		}
		mailpitGet(t, api+"/api/v1/messages?limit=200", &list)
		ids := make([]string, 0, len(list.Messages))
		for _, m := range list.Messages {
			ids = append(ids, m.MessageID)
		}
		return ids
	}})
}
