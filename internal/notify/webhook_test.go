package notify_test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/notify"
	"koro/internal/platform/db"
	"koro/internal/platform/inbox"
	"koro/internal/platform/obs"
	"koro/internal/platform/problem"
	"koro/internal/platform/queue"
)

var resendSecret = []byte("koro-test-svix-secret-32-bytes!!")

const (
	testTopicARN = "arn:aws:sns:eu-central-1:123456789012:koro-ses-events"
	testCertURL  = "https://sns.eu-central-1.amazonaws.com/SimpleNotificationService-test.pem"
)

// snsKey is a test RSA key with its self-signed certificate (the fetcher
// stands in for the HTTPS download).
var snsKey = sync.OnceValues(func() (*rsa.PrivateKey, *x509.Certificate) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "sns.amazonaws.com"},
		NotBefore: t0.Add(-time.Hour), NotAfter: t0.Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		panic(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return k, c
})

type fakeCerts struct{ fetched []string }

func (f *fakeCerts) Fetch(_ context.Context, u string) (*x509.Certificate, error) {
	f.fetched = append(f.fetched, u)
	_, c := snsKey()
	return c, nil
}

type webhookFixture struct {
	fixture
	in *inbox.Inbox
	q  *queue.Mem
}

func newWebhookFixture(t *testing.T) webhookFixture {
	t.Helper()
	q := queue.NewMem()
	var in *inbox.Inbox
	f := newFixture(t, func(d *notify.Deps) {
		in = inbox.New(inbox.Deps{Store: inbox.NewMemStore(), Tx: db.NopTx{}, Queue: q, Clock: d.Clock, Log: obs.Discard()})
		d.Inbox = in
		d.Webhooks = notify.WebhookConfig{ResendSecret: resendSecret, SESTopicARNs: []string{testTopicARN}, SNSCerts: &fakeCerts{}}
	})
	if err := in.Register(f.mod.Webhook); err != nil {
		t.Fatal(err)
	}
	return webhookFixture{fixture: f, in: in, q: q}
}

// process runs every enqueued inbox task (the worker), each task once per
// enqueue like asynq.
func (w webhookFixture) process(t *testing.T) {
	t.Helper()
	for _, task := range w.q.Tasks() {
		if task.Type != inbox.ProcessTaskName {
			continue
		}
		if err := w.in.Task().Handle(obs.WithTaskID(t.Context(), task.ID), task.Payload); err != nil {
			t.Fatalf("process %s: %v", task.ID, err)
		}
	}
	w.q.Reset()
}

func resendHeaders(id string, at time.Time, body []byte) map[string]string {
	ts := strconv.FormatInt(at.Unix(), 10)
	return map[string]string{"svix-id": id, "svix-timestamp": ts, "svix-signature": notify.SvixSignature(resendSecret, id, ts, body)}
}

func resendBounce(to string) []byte {
	return []byte(`{"type":"email.bounced","created_at":"2026-10-03T09:00:00Z","data":{"email_id":"re_1","to":["` + to +
		`"],"bounce":{"type":"Permanent","message":"mailbox does not exist"}}}`)
}

func TestWebhook_InvalidSignature_401(t *testing.T) {
	t.Parallel()
	w := newWebhookFixture(t)
	body := resendBounce("ayse.yilmaz@ogrenci.edu.tr")
	good := resendHeaders("msg_1", t0, body)

	cases := map[string]struct {
		provider string
		headers  map[string]string
		body     []byte
	}{
		"tampered body":    {notify.WebhookResend, good, append([]byte(" "), body...)},
		"wrong secret":     {notify.WebhookResend, map[string]string{"svix-id": "msg_1", "svix-timestamp": good["svix-timestamp"], "svix-signature": notify.SvixSignature([]byte("another-secret-of-32-bytes!!!!!!"), "msg_1", good["svix-timestamp"], body)}, body},
		"no signature":     {notify.WebhookResend, map[string]string{"svix-id": "msg_1", "svix-timestamp": good["svix-timestamp"]}, body},
		"stale timestamp":  {notify.WebhookResend, resendHeaders("msg_1", t0.Add(-10*time.Minute), body), body},
		"other id":         {notify.WebhookResend, map[string]string{"svix-id": "msg_2", "svix-timestamp": good["svix-timestamp"], "svix-signature": good["svix-signature"]}, body},
		"unsigned SNS":     {notify.WebhookSES, nil, snsBody(t, snsNotification("Bounce", "x@koro.test"), nil, func(e map[string]any) { e["Signature"] = "AAAA" })},
		"foreign topic":    {notify.WebhookSES, nil, snsBody(t, snsNotification("Bounce", "x@koro.test"), nil, func(e map[string]any) { e["TopicArn"] = "arn:aws:sns:eu-central-1:999999999999:evil" })},
		"cert off AWS":     {notify.WebhookSES, nil, snsBody(t, snsNotification("Bounce", "x@koro.test"), nil, func(e map[string]any) { e["SigningCertURL"] = "https://evil.example/cert.pem" })},
		"signature v1":     {notify.WebhookSES, nil, snsBody(t, snsNotification("Bounce", "x@koro.test"), nil, func(e map[string]any) { e["SignatureVersion"] = "1" })},
		"unknown provider": {"mailgun", good, body},
	}
	for name, c := range cases {
		err := w.svc.ReceiveEmailWebhook(t.Context(), c.provider, c.headers, c.body)
		var pe *problem.Error
		if !errors.Is(err, problem.ErrUnauthenticated) || !errors.As(err, &pe) || pe.Status() != 401 {
			t.Errorf("%s: error = %v, want 401 unauthenticated", name, err)
		}
	}
	if len(w.q.Tasks()) != 0 {
		t.Fatalf("a rejected delivery reached the inbox: %d tasks", len(w.q.Tasks()))
	}
	// The logs name the provider and reason, never the address.
	if strings.Contains(w.logs.String(), "ayse.yilmaz") {
		t.Fatal("the webhook rejection logged the address")
	}
	// The genuine delivery passes.
	if err := w.svc.ReceiveEmailWebhook(t.Context(), notify.WebhookResend, good, body); err != nil {
		t.Fatalf("valid delivery: %v", err)
	}
}

func TestWebhook_Bounce_AddsSuppression(t *testing.T) {
	t.Parallel()
	w := newWebhookFixture(t)
	// An e-mail goes out, then its permanent bounce arrives over SES/SNS.
	id := mustSend(t, w.fixture, testMessage("k1"))
	sent := mustGet(t, w.store, id)
	body := snsBody(t, snsNotification("Bounce", sent.ProviderMessageID), nil, nil)
	if err := w.svc.ReceiveEmailWebhook(t.Context(), notify.WebhookSES, map[string]string{"x-amz-sns-message-type": "Notification"}, body); err != nil {
		t.Fatal(err)
	}
	w.process(t)

	if yes, _ := w.store.IsSuppressed(t.Context(), index("ayse.yilmaz@ogrenci.edu.tr")); !yes {
		t.Fatal("the bounced address is not suppressed")
	}
	s, _ := w.store.GetSuppression(t.Context(), index("ayse.yilmaz@ogrenci.edu.tr"))
	if s.Reason != notify.SuppressHardBounce || s.Source != notify.SourceProviderWebhook {
		t.Fatalf("suppression = %+v, want hard_bounce from provider_webhook", s)
	}
	if d := mustGet(t, w.store, id); d.Status != notify.StatusBounced || sent.ProviderMessageID == "" {
		t.Fatalf("delivery = %+v, want bounced (matched by the Message-ID)", d)
	}
	// The next e-mail to the address is not sent but recorded.
	next, err := w.svc.Send(workerCtx(t), testMessage("k2"))
	if err != nil {
		t.Fatal(err)
	}
	if d := mustGet(t, w.store, next); d.Status != notify.StatusSuppressed || len(w.tr.Sent()) != 1 {
		t.Fatalf("next delivery = %+v with %d e-mails, want suppressed and still one e-mail", d, len(w.tr.Sent()))
	}

	// Resend complaint: complaint suppression; a transient bounce adds nothing.
	cbody := []byte(`{"type":"email.complained","data":{"email_id":"re_2","to":["Mehmet <mehmet@koro.test>"]}}`)
	tbody := []byte(`{"type":"email.bounced","data":{"email_id":"re_3","to":["temp@koro.test"],"bounce":{"type":"Transient"}}}`)
	for i, b := range [][]byte{cbody, tbody} {
		if err := w.svc.ReceiveEmailWebhook(t.Context(), notify.WebhookResend, resendHeaders("msg_c"+strconv.Itoa(i), t0, b), b); err != nil {
			t.Fatal(err)
		}
	}
	w.process(t)
	if c, err := w.store.GetSuppression(t.Context(), index("mehmet@koro.test")); err != nil || c.Reason != notify.SuppressComplaint {
		t.Fatalf("complaint suppression = (%+v, %v)", c, err)
	}
	if yes, _ := w.store.IsSuppressed(t.Context(), index("temp@koro.test")); yes {
		t.Fatal("a transient bounce suppressed the address")
	}
}

func TestWebhook_DuplicateEvent_ProcessedOnce(t *testing.T) {
	t.Parallel()
	w := newWebhookFixture(t)
	body := resendBounce("dup@koro.test")
	h := resendHeaders("msg_dup", t0, body)
	for range 3 {
		if err := w.svc.ReceiveEmailWebhook(t.Context(), notify.WebhookResend, h, body); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(w.q.Tasks()); n != 1 {
		t.Fatalf("%d processing tasks for one event, want 1 (dedupe by svix-id)", n)
	}
	w.process(t)
	// A redelivery after processing is stored once and not processed again.
	if err := w.svc.ReceiveEmailWebhook(t.Context(), notify.WebhookResend, h, body); err != nil {
		t.Fatal(err)
	}
	if n := len(w.q.Tasks()); n != 0 {
		t.Fatalf("a processed event was enqueued again (%d tasks)", n)
	}
	if !strings.Contains(w.logs.String(), `"suppressed":1`) || strings.Count(w.logs.String(), "email webhook processed") != 1 {
		t.Fatalf("logs = %s, want exactly one processing that suppressed one address", w.logs.String())
	}
}

func TestWebhook_SNSSubscriptionIsNotFollowed(t *testing.T) {
	t.Parallel()
	w := newWebhookFixture(t)
	body := snsBody(t, "confirm", func(e map[string]any) {
		e["Type"] = "SubscriptionConfirmation"
		e["Token"] = "tok"
		e["SubscribeURL"] = "https://sns.eu-central-1.amazonaws.com/?Action=ConfirmSubscription"
		delete(e, "Subject")
	}, nil)
	if err := w.svc.ReceiveEmailWebhook(t.Context(), notify.WebhookSES, nil, body); err != nil {
		t.Fatal(err)
	}
	w.process(t)
	if !strings.Contains(w.logs.String(), "confirm the subscription in the AWS console") {
		t.Fatal("the subscription message was not reported")
	}
}

func TestValidSNSCertURL(t *testing.T) {
	t.Parallel()
	for _, u := range []string{testCertURL, "https://sns.us-east-1.amazonaws.com/x.pem"} {
		if err := notify.ValidSNSCertURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	for _, u := range []string{
		"http://sns.eu-central-1.amazonaws.com/x.pem", "https://sns.eu-central-1.amazonaws.com.evil.io/x.pem",
		"https://evil@sns.eu-central-1.amazonaws.com/x.pem", "https://sns.eu-central-1.amazonaws.com:8443/x.pem",
		"https://sns.eu-central-1.amazonaws.com/x.pem?a=1", "https://sns.eu-central-1.amazonaws.com/x.txt",
		"https://s3.amazonaws.com/x.pem", "https://169.254.169.254/latest/x.pem",
	} {
		if notify.ValidSNSCertURL(u) == nil {
			t.Errorf("%s accepted", u)
		}
	}
}

// snsNotification is the SES notification JSON of a bounce of the test
// address; messageID is the Message-ID header of the bounced e-mail.
func snsNotification(kind, messageID string) string {
	n := map[string]any{
		"notificationType": kind,
		"mail":             map[string]any{"messageId": "ses-1", "commonHeaders": map[string]any{"messageId": "<" + messageID + ">"}},
		"bounce": map[string]any{"bounceType": "Permanent",
			"bouncedRecipients": []map[string]any{{"emailAddress": "ayse.yilmaz@ogrenci.edu.tr"}}},
	}
	b, _ := json.Marshal(n)
	return string(b)
}

// snsBody builds an SNS message carrying message: before edits the fields
// that are signed, after breaks the signed message.
func snsBody(t *testing.T, message string, before, after func(map[string]any)) []byte {
	t.Helper()
	e := map[string]any{
		"Type": "Notification", "MessageId": "sns-" + strconv.Itoa(len(message)), "TopicArn": testTopicARN,
		"Subject": "Amazon SES Email Event Notification", "Message": message, "Timestamp": t0.Format(time.RFC3339),
		"SignatureVersion": "2", "SigningCertURL": testCertURL,
	}
	if before != nil {
		before(e)
	}
	k, _ := snsKey()
	sum := sha256.Sum256([]byte(notify.SNSStringToSign(t, e)))
	sig, err := rsa.SignPKCS1v15(rand.Reader, k, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	e["Signature"] = base64.StdEncoding.EncodeToString(sig)
	if after != nil {
		after(e)
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
