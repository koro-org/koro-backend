package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"gorm.io/gorm"

	"koro/internal/consent"
	"koro/internal/consent/iystest"
	"koro/internal/notify"
	"koro/internal/notify/pushtest"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/openbao"
	"koro/internal/platform/crypto/openbao/openbaotest"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/db"
	"koro/internal/platform/obs"
)

const testKEKs = "pilot1:AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

// TestWire_CryptoKeys: with OpenBao configured every new value is wrapped by
// Transit (key id in the header), the local KEK still opens older values,
// /readyz gets an "openbao" check that fails closed with OpenBao down, and
// modules declare their encrypted columns for crypto.rewrap.
func TestWire_CryptoKeys(t *testing.T) {
	t.Parallel()
	bao := openbaotest.NewFake(t, "koro-kek")
	c := newTestContainer(t)
	c.infra.platform.Crypto = config.Crypto{
		KEKs: config.NewSecret(testKEKs), OpenBaoAddr: bao.Server.URL, OpenBaoToken: config.NewSecret(openbaotest.FakeToken),
		OpenBaoMount: "transit", OpenBaoKey: "koro-kek", OpenBaoTimeout: 2 * time.Second,
	}
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	env, err := c.keys.envelope()
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	sealed, err := env.Seal(ctx, "identity.mfa_secret/u", []byte("s"))
	if err != nil {
		t.Fatal(err)
	}
	if kid, _ := crypto.KeyID(sealed); kid != "bao.koro-kek.v1" {
		t.Fatalf("new value under %q, want the OpenBao key", kid)
	}
	keys, _ := crypto.ParseKeyring(testKEKs)
	kek, _ := crypto.NewLocalKEK(keys)
	localEnv, _ := crypto.NewEnvelope(kek)
	old, _ := localEnv.Seal(ctx, "identity.mfa_secret/u", []byte("old"))
	if pt, err := env.Open(ctx, "identity.mfa_secret/u", old); err != nil || string(pt) != "old" {
		t.Fatalf("pilot value = %q, %v", pt, err)
	}

	var check func(context.Context) error
	for _, ch := range c.readyChecks {
		if ch.Name == "openbao" {
			check = ch.Run
		}
	}
	if check == nil || check(ctx) != nil {
		t.Fatal("no working openbao readiness check")
	}
	bao.SetDown(true)
	if err := check(ctx); !errors.Is(err, crypto.ErrUnavailable) {
		t.Fatalf("readiness with OpenBao down = %v", err)
	}
	if _, err := env.Seal(ctx, "p", []byte("x")); !errors.Is(err, crypto.ErrUnavailable) {
		t.Fatalf("Seal with OpenBao down = %v, want fail closed", err)
	}
	bao.SetDown(false)

	col := rewrap.Column{Name: "identity.mfa_totp.secret_enc", Table: "mfa_totp", Key: "user_id", Column: "secret_enc",
		PurposePrefix: "identity.mfa_secret/", PurposeColumn: "user_id"}
	if err := c.keys.addRewrap(col); err != nil {
		t.Fatal(err)
	}
	if err := c.keys.addRewrap(col); err == nil {
		t.Fatal("a column was declared twice")
	}
	if err := c.keys.addRewrap(rewrap.Column{Name: "x", Table: "t;", Key: "id", Column: "c", PurposePrefix: "p"}); err == nil {
		t.Fatal("an invalid identifier was accepted")
	}
}

// Without any key (wiring tests, `koro openapi`) no envelope exists, so a
// module that needs one fails its build instead of sealing with nothing;
// with the local KEK only, values are wrapped locally.
func TestWire_CryptoKeys_NoKeyAndLocal(t *testing.T) {
	t.Parallel()
	c := newTestContainer(t)
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if env, err := c.keys.envelope(); env != nil || !errors.Is(err, errNoKey) {
		t.Fatalf("envelope without keys = %v, %v", env, err)
	}
	local := newTestContainer(t)
	local.infra.platform.Crypto = config.Crypto{KEKs: config.NewSecret(testKEKs)}
	if err := local.build(); err != nil {
		t.Fatal(err)
	}
	env, err := local.keys.envelope()
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := env.Seal(t.Context(), "p", []byte("x"))
	if kid, _ := crypto.KeyID(sealed); kid != "pilot1" {
		t.Fatalf("local key id = %q", kid)
	}
	for _, ch := range local.readyChecks {
		if ch.Name == "openbao" {
			t.Fatal("openbao readiness check without OpenBao")
		}
	}
}

// The crypto jobs (01 §7 additions): crypto.rewrap hourly at :50 on the low
// queue whenever an envelope and the database exist, crypto.renew_token
// hourly at :07 with OpenBao only.
func TestWire_CryptoJobs(t *testing.T) {
	t.Parallel()
	bao := openbaotest.NewFake(t, "koro-kek")
	jobsOf := func(cfg config.Crypto) []string {
		c := newTestContainer(t)
		c.infra.platform.Crypto = cfg
		if err := c.keys.build(c); err != nil {
			t.Fatal(err)
		}
		c.infra.db = &db.DB{ORM: &gorm.DB{}, Tx: db.NopTx{}} // tasks only; nothing runs
		c.keys.tasks(c)
		var out []string
		for _, j := range c.infra.jobs.Jobs() {
			if err := j.Validate(); err != nil {
				t.Fatal(err)
			}
			out = append(out, j.Name+" "+j.Cron+" "+j.Queue)
		}
		return out
	}
	withBao := jobsOf(config.Crypto{OpenBaoAddr: bao.Server.URL, OpenBaoToken: config.NewSecret(openbaotest.FakeToken),
		OpenBaoMount: "transit", OpenBaoKey: "koro-kek", OpenBaoTimeout: time.Second})
	if want := []string{"crypto.rewrap 50 * * * * low", "crypto.renew_token 7 * * * * default"}; !slices.Equal(withBao, want) {
		t.Fatalf("jobs with OpenBao = %q, want %q", withBao, want)
	}
	if local := jobsOf(config.Crypto{KEKs: config.NewSecret(testKEKs)}); !slices.Equal(local, []string{"crypto.rewrap 50 * * * * low"}) {
		t.Fatalf("jobs with the local KEK = %q", local)
	}
	if none := jobsOf(config.Crypto{}); len(none) != 0 {
		t.Fatalf("jobs without keys = %q", none)
	}
}

type renewer struct {
	renewed bool
	err     error
}

func (r renewer) RenewToken(context.Context) (bool, error) { return r.renewed, r.err }

func TestWire_RenewTokenJob(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	j := renewTokenJob(renewer{renewed: true}, log)
	if j.Name != "crypto.renew_token" || j.Validate() != nil {
		t.Fatalf("job = %+v (%v)", j, j.Validate())
	}
	if err := j.Handle(t.Context()); err != nil {
		t.Fatal(err)
	}
	down := renewTokenJob(renewer{err: fmt.Errorf("%w: openbao: HTTP 503", crypto.ErrUnavailable)}, log)
	if err := down.Handle(t.Context()); err == nil || !strings.Contains(logs.String(), `"alarm":true`) {
		t.Fatalf("failed renewal = %v; log %s", err, logs.String())
	}
}

// TestWire_ExternalAdaptersFromConfig: the push transport and the İYS client
// are built from their settings; PUSH_REQUIRED without a provider stops the
// build; the İYS recipient comes from identity at call time.
func TestWire_ExternalAdaptersFromConfig(t *testing.T) {
	t.Parallel()
	fcm := pushtest.NewFCM(t)
	apns := pushtest.NewAPNs(t)
	c := newTestContainer(t)
	c.notify.cfg.PushFCMProjectID, c.notify.cfg.PushFCMCredentials = fcm.ProjectID, config.NewSecret(string(fcm.Credentials))
	c.notify.cfg.PushAPNsKey = config.NewSecret(string(apns.KeyPEM))
	c.notify.cfg.PushAPNsKeyID, c.notify.cfg.PushAPNsTeamID, c.notify.cfg.PushAPNsTopic = apns.KeyID, apns.TeamID, apns.Topic
	c.consent.iys = nil
	c.consent.cfg = &config.Consent{IYSBaseURL: "https://api.iys.org.tr", IYSUsername: "u", IYSPassword: config.NewSecret("p"),
		IYSCode: "1", IYSBrandCode: "2", IYSRecipientType: "BIREYSEL", IYSRequired: true}
	if err := c.build(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.notify.deps.Push.(*notify.PushTransport); !ok {
		t.Fatalf("push sender = %T, want *notify.PushTransport", c.notify.deps.Push)
	}
	_, err := iysRecipients{c: c}.IYSRecipient(t.Context(), uuid.NewV7(), consent.IYSEmail)
	if !errors.Is(err, consent.ErrIYSRecipientGone) {
		t.Fatalf("recipient of an unknown user = %v, want gone", err)
	}
	if _, err := (iysRecipients{c: c}).IYSRecipient(t.Context(), uuid.NewV7(), consent.IYSSMS); !errors.Is(err, consent.ErrIYSRecipientGone) {
		t.Fatalf("sms recipient = %v", err)
	}

	required := newTestContainer(t)
	required.notify.cfg.PushRequired = true
	if err := required.build(); err == nil || !strings.Contains(err.Error(), "PUSH_REQUIRED") {
		t.Fatalf("PUSH_REQUIRED without providers = %v", err)
	}
	broken := newTestContainer(t)
	broken.notify.cfg.PushFCMProjectID, broken.notify.cfg.PushFCMCredentials = fcm.ProjectID, config.NewSecret(`{"type":"service_account"}`)
	if err := broken.build(); err == nil || strings.Contains(err.Error(), "service_account\"}") {
		t.Fatalf("broken FCM credentials = %v", err)
	}
}

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type staticRecipients map[uuid.UUID]string

func (s staticRecipients) IYSRecipient(_ context.Context, id uuid.UUID, _ consent.IYSChannel) (string, error) {
	if s[id] == "" {
		return "", consent.ErrIYSRecipientGone
	}
	return s[id], nil
}

// TestSecrets_NotLogged drives every external adapter of BE-055 through
// success and failure with the production log handler (obs) and checks that
// no credential, access token, device token, recipient address or key
// material reaches a log line, an error or a printed configuration.
func TestSecrets_NotLogged(t *testing.T) {
	t.Parallel()
	var logs lockedBuffer
	log := obs.NewLogger(&logs, obs.LoggerOptions{Level: slog.LevelDebug})
	clk := clock.NewFake(time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC))
	ctx := t.Context()
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}

	// OpenBao: a wrong token, an outage, a rewrap-style failure.
	bao := openbaotest.NewFake(t, "koro-kek")
	const wrongToken = "s.WrongOpenBaoToken0123456789"
	bad, _ := openbao.New(openbao.Config{Addr: bao.Server.URL, Token: wrongToken, Key: "koro-kek", Clock: clk})
	_, _, err := bad.Wrap(ctx, bytes.Repeat([]byte{0x5a}, 32), []byte("aad"))
	note(err)
	note(bad.Check(ctx))
	good, _ := openbao.New(openbao.Config{Addr: bao.Server.URL, Token: openbaotest.FakeToken, Key: "koro-kek", Clock: clk})
	env, _ := crypto.NewEnvelope(good)
	bao.SetDown(true)
	_, err = env.Seal(ctx, "p", []byte("plain-secret-value"))
	note(err)
	bao.SetDown(false)
	renew := renewTokenJob(bad, log)
	note(renew.Handle(ctx))

	// Push: FCM (service account, access token) and APNs (.p8 key, provider
	// token, device tokens) with dead tokens and an outage.
	fcm := pushtest.NewFCM(t)
	apns := pushtest.NewAPNs(t)
	store := notify.NewMemStore()
	user := uuid.NewV7()
	const fcmDevice, apnsDevice = "fcm-device-token-SECRET-1", "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	for _, d := range []struct {
		pl  notify.PushPlatform
		tok string
	}{{notify.PushAndroid, fcmDevice}, {notify.PushIOS, apnsDevice}} {
		_, _ = store.UpsertPushToken(ctx, notify.PushToken{ID: uuid.NewV7(), UserID: user, Platform: d.pl, Token: d.tok,
			TokenSHA256: bytes.Repeat([]byte{byte(len(d.tok))}, 32), CreatedAt: clk.Now(), LastSeenAt: clk.Now()})
	}
	f, err := notify.NewFCM(notify.FCMConfig{ProjectID: fcm.ProjectID, CredentialsJSON: fcm.Credentials, BaseURL: fcm.Server.URL,
		Clock: clk, HTTPClient: fcm.Client()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := notify.NewAPNs(notify.APNsConfig{KeyPEM: apns.KeyPEM, KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: apns.Topic,
		BaseURL: apns.Server.URL, Clock: clk, HTTPClient: apns.Client()})
	if err != nil {
		t.Fatal(err)
	}
	pt, _ := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, FCM: f, APNs: a, Clock: clk, Log: log})
	msg := notify.PushMessage{DeliveryID: uuid.NewV7(), UserID: user, Kind: "k", Title: "Yeni bir bildirimin var"}
	_, err = pt.SendPush(ctx, msg)
	note(err)
	fcm.SetDown(true)
	apns.Kill(apnsDevice)
	_, err = pt.SendPush(ctx, msg)
	note(err)
	unreachable, _ := notify.NewAPNs(notify.APNsConfig{KeyPEM: apns.KeyPEM, KeyID: apns.KeyID, TeamID: apns.TeamID, Topic: apns.Topic,
		BaseURL: "https://127.0.0.1:1", Clock: clk, Timeout: time.Second})
	_, _ = store.UpsertPushToken(ctx, notify.PushToken{ID: uuid.NewV7(), UserID: user, Platform: notify.PushIOS,
		Token: strings.Repeat("cd", 32), TokenSHA256: bytes.Repeat([]byte{7}, 32), CreatedAt: clk.Now(), LastSeenAt: clk.Now()})
	pt2, _ := notify.NewPushTransport(notify.PushTransportConfig{Tokens: store, APNs: unreachable, Clock: clk, Log: log})
	_, err = pt2.SendPush(ctx, msg)
	note(err)

	// İYS: password sign-in, access token, a refused record that echoes the
	// address, an outage.
	iys := iystest.New(t)
	ada := uuid.NewV7()
	const address = "ada.secret@ogrenci.edu.tr"
	client, err := consent.NewIYSHTTP(consent.IYSHTTPConfig{BaseURL: iys.URL, Username: iystest.Username, Password: iystest.Password,
		IYSCode: iystest.IYSCode, BrandCode: iystest.BrandCode, Recipients: staticRecipients{ada: address}, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	rec := consent.IYSRecord{SubmissionID: uuid.NewV7(), ConsentID: uuid.NewV7(), UserID: ada, Channel: consent.IYSEmail,
		Action: consent.Granted, Source: consent.ChannelWeb, OccurredAt: clk.Now()}
	_, err = client.Submit(ctx, rec)
	note(err)
	iys.Reject(address)
	_, err = client.Submit(ctx, rec)
	note(err)
	iys.SetDown(true)
	_, err = client.Submit(ctx, rec)
	note(err)
	wrongPass, _ := consent.NewIYSHTTP(consent.IYSHTTPConfig{BaseURL: iys.URL, Username: iystest.Username, Password: "wrong-iys-password-987",
		IYSCode: iystest.IYSCode, BrandCode: iystest.BrandCode, Recipients: staticRecipients{ada: address}, Clock: clk})
	iys.SetDown(false)
	_, err = wrongPass.Submit(ctx, rec)
	note(err)
	for _, e := range errs {
		log.Error("adapter error", slog.String("error", e))
	}

	// Configuration printed with secrets set.
	cfgs := fmt.Sprintf("%+v %+v %+v", config.Crypto{OpenBaoToken: config.NewSecret(wrongToken), KEKs: config.NewSecret(testKEKs)},
		config.Notify{PushFCMCredentials: config.NewSecret(fcm.PrivateKey), PushAPNsKey: config.NewSecret(string(apns.KeyPEM))},
		config.Consent{IYSPassword: config.NewSecret(iystest.Password)})
	log.Info("config", slog.Any("crypto", config.Crypto{OpenBaoToken: config.NewSecret(wrongToken)}))

	if len(errs) < 8 {
		t.Fatalf("expected the failure paths to produce errors, got %d: %q", len(errs), errs)
	}
	out := logs.String() + strings.Join(errs, "\n") + cfgs
	secrets := map[string]string{
		"openbao token": wrongToken, "openbao app token": openbaotest.FakeToken, "plaintext": "plain-secret-value",
		"local KEK": testKEKs[7:], "FCM private key": "BEGIN PRIVATE KEY", "FCM access token": "ya29.fake-access",
		"FCM device token": fcmDevice, "APNs device token": apnsDevice, "APNs device token 2": strings.Repeat("cd", 32),
		"İYS password": iystest.Password, "İYS wrong password": "wrong-iys-password-987", "İYS access token": "iys-access-",
		"recipient address": address,
	}
	for _, tok := range apns.ProviderTokens() {
		secrets["APNs provider token"] = tok
	}
	for what, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Errorf("%s leaked:\n%s", what, out)
		}
	}
	if len(apns.ProviderTokens()) == 0 || len(fcm.IssuedAccessTokens()) == 0 || iys.SignIns() == 0 {
		t.Fatal("the adapters did not authenticate: the test would prove nothing")
	}
}
