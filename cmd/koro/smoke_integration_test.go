//go:build integration

package main

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"koro/db/seed"
	"koro/internal/platform/storage/storagetest"
	"koro/internal/platform/testutil"
)

// TestSmoke_ClubFirstPilotFlow is the P1 smoke test (BE-033): the real
// `koro api` and `koro worker` in-process over PostgreSQL, Redis,
// S3 (SeaweedFS) and Mailpit (testcontainers), the database filled by
// `koro seed reference` and `koro seed demo`, and the club-first pilot flow
// over HTTP, each step checked by its status code and its critical side
// effect:
//
//  1. a student registers, confirms the e-mail (link from Mailpit), signs
//     in, verifies the student status with a .edu.tr address (link from
//     Mailpit) and joins an open club;
//  2. the club's admin creates an event with a capacity of two, publishes
//     its form and the event; two students register (confirmed), a third
//     one is refused (capacity); the ticket e-mail arrives with the QR as
//     an inline image, the manual code and the .ics; the door checks the
//     QR in (and refuses a second scan);
//  3. the club publishes a post, the student sees it in the feed and likes
//     it;
//  4. a company targets the students of a university (k ≥ 10, one unit of
//     its quota): a too small audience is refused, the post reaches an
//     adult student of that university and not a minor;
//  5. KVKK: the DPO (no demo password, 06 H-66: opened by a password
//     reset) answers the student's data export, which is
//     downloaded (a zip from S3), then the account is erased: the sessions
//     end, the account is a tombstone and signing in fails.
func TestSmoke_ClubFirstPilotFlow(t *testing.T) {
	t.Parallel()

	// --- infrastructure: PostgreSQL, Redis, S3, Mailpit ---
	d := testutil.NewDB(t)
	rdb := testutil.NewRedis(t)
	s3srv := storagetest.Start(t)
	s3 := storagetest.Client(t)
	filesBucket, quarantine := storagetest.Bucket(t, s3), storagetest.Bucket(t, s3)
	smtpHost, smtpPort, mailAPI := startMailpit(t)

	// The 3-minute budget (BE-033) measures the flow, not the container
	// start-up: under load (a busy machine or a CI runner) the containers
	// alone can take minutes (OPS-001). The clock starts once they are ready.
	start := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()

	environ := seedEnviron(t, d)
	for k, v := range map[string]string{
		"HTTP_ADDR": "127.0.0.1:0", "PUBLIC_BASE_URL": "http://localhost:3000", "TRUSTED_ORIGINS": "http://localhost:3000",
		"SHUTDOWN_DELAY": "1ms", "SHUTDOWN_TIMEOUT": "5s", "RATE_LIMIT_ENABLED": "false", "REDIS_URL": redisURL(rdb),
		"WORKER_HEALTH_ADDR": "127.0.0.1:0", "WORKER_SHUTDOWN_TIMEOUT": "5s", "WORKER_SCHEDULER_ENABLED": "false",
		"OUTBOX_RELAY_IDLE": "50ms", "DB_MAX_CONNS": "8",
		"S3_ENDPOINT": s3srv.Endpoint, "S3_ACCESS_KEY": storagetest.AccessKey, "S3_SECRET_KEY": storagetest.SecretKey,
		"S3_REGION": storagetest.Region, "S3_BUCKET_FILES": filesBucket, "S3_BUCKET_QUARANTINE": quarantine,
		"SMTP_HOST": smtpHost, "SMTP_PORT": strconv.Itoa(smtpPort), "SMTP_TLS": "none",
		"IDENTITY_LINK_BASE_URL": "http://localhost:3000",
	} {
		environ[k] = v
	}

	// --- data: reference + synthetic demo; a synthetic .edu.tr domain for
	// the main university (reference data, as `koro seed reference` writes
	// it; no real address is ever used) ---
	seedCmd(t, environ, "reference")
	seedCmd(t, environ, "demo", "-password", demoPassword)
	var mainUni, mainClub, company, segment uuid.UUID
	q := func(sql string, dst any, args ...any) {
		t.Helper()
		if err := d.Pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	q(`SELECT u.id FROM universities u ORDER BY u.yok_code LIMIT 1`, &mainUni)
	q(`SELECT o.id FROM users us JOIN org_members m ON m.user_id = us.id AND m.role = 'club_admin' JOIN organizations o ON o.id = m.org_id
		WHERE us.email = $1`, &mainClub, seed.ClubAdminEmail)
	q(`SELECT m.org_id FROM users us JOIN org_members m ON m.user_id = us.id AND m.role = 'company_owner' WHERE us.email = $1`,
		&company, seed.CompanyEmail)
	q(`SELECT s.id FROM segments s WHERE s.org_id = $1 AND s.audience_keys = ARRAY['uni:' || $2::text]`, &segment, company, mainUni.String())
	if _, err := d.Pool.Exec(ctx, `INSERT INTO university_email_domains (domain, university_id, domain_kind) VALUES ('example.edu.tr', $1, 'student')`,
		mainUni); err != nil {
		t.Fatal(err)
	}

	// --- processes: koro api + koro worker ---
	api := startProcess(t, ctx, "api listening", "addr", func(ctx context.Context, stop context.CancelFunc, w io.Writer) error {
		return runAPI(ctx, stop, environ, w)
	})
	startProcess(t, ctx, "worker started", "", func(ctx context.Context, stop context.CancelFunc, w io.Writer) error {
		return runWorker(ctx, stop, environ, w)
	})
	c := &smokeClient{t: t, ctx: ctx, base: "http://" + api, http: &http.Client{Timeout: 20 * time.Second}}
	mail := &mailpit{t: t, ctx: ctx, api: mailAPI}
	waitUntil(t, "api ready", func() bool { return c.status(http.MethodGet, "/readyz") == http.StatusOK })

	// The demo answers at once (BE-033 acceptance): the public event list
	// and a demo student's feed are full.
	if evs, _ := c.do(http.MethodGet, "/api/v1/events?limit=50", "", nil, nil, http.StatusOK)["items"].([]any); len(evs) < 10 {
		t.Fatalf("anonymous /api/v1/events after the demo seed: %d items", len(evs))
	}
	if feed, _ := c.do(http.MethodGet, "/api/v1/feed?limit=50", c.login(seed.StudentEmail, demoPassword), nil, nil,
		http.StatusOK)["items"].([]any); len(feed) < 20 {
		t.Fatalf("a demo student's /api/v1/feed after the demo seed: %d items", len(feed))
	}

	// MFA roles first (their TOTP steps then have time to advance). A
	// platform role account has no demo password (06 H-66): the DPO opens
	// the account by a password reset (link from Mailpit) and enrols TOTP;
	// the company owner signs in with the demo password and enrols TOTP.
	if _, st := c.try(http.MethodPost, "/api/v1/auth/sessions", "", map[string]any{"email": seed.DPOEmail,
		"password": demoPassword}); st != http.StatusUnauthorized {
		t.Fatalf("the DPO signed in with the demo password: %d (06 H-66)", st)
	}
	c.do(http.MethodPost, "/api/v1/auth/password-resets", "", map[string]any{"email": seed.DPOEmail}, nil, http.StatusAccepted)
	c.do(http.MethodPost, "/api/v1/auth/password-resets/confirm", "", map[string]any{"token": mail.token(seed.DPOEmail),
		"new_password": demoPassword}, nil, http.StatusNoContent)
	dpo := c.login(seed.DPOEmail, demoPassword)
	dpoTOTP := c.enrolTOTP(dpo, demoPassword)
	owner := c.login(seed.CompanyEmail, demoPassword)
	c.enrolTOTP(owner, demoPassword)

	// --- 1. registration → e-mail → .edu.tr verification → club ---
	const studentEmail, studentPassword = "yeni.ogrenci@example.test", "Koro-Duman-Testi-2026!"
	notices := c.do(http.MethodGet, "/api/v1/notices/current?audience=student&kind=privacy_notice", "", nil, nil, http.StatusOK)
	items, _ := notices["items"].([]any)
	if len(items) == 0 {
		t.Fatal("no current privacy notice (the demo seed publishes one)")
	}
	noticeID := items[0].(map[string]any)["notice_version_id"].(string)
	// BE-118: the mandatory terms of use box (the demo seed publishes them).
	terms := c.do(http.MethodGet, "/api/v1/notices/current?audience=student&kind=terms_of_use", "", nil, nil, http.StatusOK)
	termsItems, _ := terms["items"].([]any)
	if len(termsItems) == 0 {
		t.Fatal("no current terms of use (the demo seed publishes them)")
	}
	termsID := termsItems[0].(map[string]any)["notice_version_id"].(string)
	c.do(http.MethodPost, "/api/v1/auth/register", "", map[string]any{
		"email": studentEmail, "password": studentPassword, "first_name": "Deniz", "last_name": "Yılmaz",
		"birth_date": "2004-05-20", "locale": "tr-TR", "signup_persona": "student", "privacy_notice_version_id": noticeID,
		"terms_notice_version_id": termsID,
	}, nil, http.StatusAccepted)
	token := mail.token(studentEmail)
	c.do(http.MethodPost, "/api/v1/auth/email-verifications/confirm", "", map[string]any{"token": token}, nil, http.StatusNoContent)
	student := c.login(studentEmail, studentPassword)
	const eduEmail = "deniz.yilmaz@ogr.example.edu.tr"
	c.do(http.MethodPost, "/api/v1/me/edu-verifications", student, map[string]any{"edu_email": eduEmail}, nil, http.StatusAccepted)
	c.do(http.MethodPost, "/api/v1/auth/edu-verifications/confirm", "", map[string]any{"token": mail.token(eduEmail)}, nil, http.StatusNoContent)
	me := c.do(http.MethodGet, "/api/v1/me", student, nil, nil, http.StatusOK)
	if st, _ := me["student"].(map[string]any); st == nil || st["university_id"] != mainUni.String() {
		t.Fatalf("after the .edu.tr verification: student = %v", me["student"])
	}
	if tm, _ := me["terms"].(map[string]any); tm == nil || tm["acceptance_required"] != false || tm["accepted_notice_version_id"] != termsID {
		t.Fatalf("after the registration: terms = %v", me["terms"])
	}
	join := c.do(http.MethodPost, "/api/v1/clubs/"+mainClub.String()+"/join-requests", student, map[string]any{}, nil, http.StatusCreated)
	if join["status"] != "active" {
		t.Fatalf("join an open club: %v", join)
	}

	// --- 2. event → form → registration (capacity) → QR e-mail → check-in ---
	clubAdmin := c.login(seed.ClubAdminEmail, demoPassword)
	startsAt := time.Now().UTC().Add(72 * time.Hour).Truncate(time.Minute)
	ev := c.do(http.MethodPost, "/api/v1/orgs/"+mainClub.String()+"/events", clubAdmin, map[string]any{
		"title": "Duman Testi Atölyesi", "category": "workshop", "format": "in_person", "city": "Ankara",
		"location_name": "Demo Salonu", "starts_at": startsAt.Format(time.RFC3339), "ends_at": startsAt.Add(2 * time.Hour).Format(time.RFC3339),
		"capacity": 2, "checkin_enabled": true, "visibility": "public", "registration_mode": "auto",
	}, nil, http.StatusCreated)
	eventID := ev["event_id"].(string)
	_, hdr := c.doH(http.MethodGet, "/api/v1/events/"+eventID+"/form/draft", clubAdmin, nil, nil, http.StatusOK)
	c.do(http.MethodPut, "/api/v1/events/"+eventID+"/form/draft", clubAdmin, map[string]any{"draft_schema": map[string]any{
		"schema_version": 1, "fields": []any{map[string]any{"key": "beklenti", "type": "short_text",
			"label": map[string]any{"tr": "Etkinlikten beklentin"}, "required": false}},
	}}, map[string]string{"If-Match": hdr.Get("ETag")}, http.StatusOK)
	form := c.do(http.MethodPost, "/api/v1/events/"+eventID+"/form/versions", clubAdmin, map[string]any{}, nil, http.StatusCreated)
	formVersion := form["form_version_id"].(string)
	if pub := c.do(http.MethodPost, "/api/v1/events/"+eventID+"/publish", clubAdmin, nil, nil, http.StatusOK); pub["status"] != "published" {
		t.Fatalf("publish: %v", pub)
	}
	register := func(tok string, want int) map[string]any {
		return c.do(http.MethodPost, "/api/v1/events/"+eventID+"/registrations", tok, map[string]any{
			"form_version_id": formVersion, "answers": map[string]any{"beklenti": "Yeni şeyler öğrenmek"},
		}, map[string]string{"Idempotency-Key": uuid.NewV7().String()}, want)
	}
	reg := register(student, http.StatusCreated)
	if reg["status"] != "confirmed" {
		t.Fatalf("registration: %v", reg)
	}
	seeded := c.login(seed.StudentEmail, demoPassword)
	register(seeded, http.StatusCreated)
	minor := c.login(seed.MinorEmail, demoPassword)
	if full := register(minor, http.StatusConflict); full["code"] != "events.capacity_full" {
		t.Fatalf("third registration on a capacity of two: %v", full)
	}
	regID := reg["registration_id"].(string)
	ticket := c.do(http.MethodGet, "/api/v1/me/registrations/"+regID+"/ticket", student, nil, nil, http.StatusOK)
	payload, code := ticket["qr_payload"].(string), ticket["manual_code"].(string)
	msg := mail.wait(studentEmail, "Biletin hazır")
	if len(msg.Inline) != 1 || msg.Inline[0].ContentType != "image/png" || msg.Inline[0].ContentID != "ticket-qr" ||
		!strings.Contains(msg.HTML, "cid:ticket-qr") {
		t.Fatalf("ticket e-mail without the inline QR: %+v", msg.Inline)
	}
	if !strings.Contains(msg.Text, code[:4]+"-"+code[4:]) || strings.Contains(msg.Text+msg.HTML, strings.TrimPrefix(payload, "KORO1:")) {
		t.Fatal("ticket e-mail: the manual code is missing or the token is in the text")
	}
	if len(msg.Attachments) != 1 || msg.Attachments[0].FileName != "etkinlik.ics" {
		t.Fatalf("ticket e-mail attachments: %+v", msg.Attachments)
	}
	scan := func() map[string]any {
		return c.do(http.MethodPost, "/api/v1/events/"+eventID+"/check-ins", clubAdmin, map[string]any{
			"client_scan_id": uuid.NewV7().String(), "method": "qr", "token": payload,
			"scanned_at": time.Now().UTC().Format(time.RFC3339),
		}, map[string]string{"Idempotency-Key": uuid.NewV7().String()}, http.StatusOK)
	}
	if r := scan(); r["result"] != "ok" {
		t.Fatalf("check-in: %v", r)
	}
	if r := scan(); r["result"] != "already_checked_in" {
		t.Fatalf("second scan: %v", r)
	}

	// --- 3. club post → feed → like ---
	post := c.do(http.MethodPost, "/api/v1/orgs/"+mainClub.String()+"/posts", clubAdmin, map[string]any{
		"kind": "text", "title": "Duman testi duyurusu", "audience_kind": "public", "body_json": tiptap("Kulübümüzün yeni atölyesine bekleriz!"),
	}, nil, http.StatusCreated)
	postID := post["post_id"].(string)
	if p := c.do(http.MethodPost, "/api/v1/posts/"+postID+"/publish", clubAdmin, nil, nil, http.StatusOK); p["status"] != "published" {
		t.Fatalf("publish post: %v", p)
	}
	if item := c.feedItem(student, postID); item == nil {
		t.Fatal("the club's post is not in the member's feed")
	}
	c.do(http.MethodPut, "/api/v1/posts/"+postID+"/like", student, nil, nil, http.StatusNoContent)
	if item := c.feedItem(student, postID); item == nil || item["like_count"] != float64(1) ||
		item["viewer"].(map[string]any)["liked"] != true {
		t.Fatalf("after the like: %v", item)
	}

	// --- 4. company targeted post (k ≥ 10, quota) ---
	var smallDept uuid.UUID
	q(`SELECT d.id FROM departments d WHERE d.university_id = $1 ORDER BY
		(SELECT count(*) FROM student_profiles sp WHERE sp.department_id = d.id AND sp.class_year = 1), d.id LIMIT 1`, &smallDept, mainUni)
	tooSmall := c.do(http.MethodPost, "/api/v1/orgs/"+company.String()+"/targeted-posts", owner, map[string]any{
		"kind": "text", "title": "Küçük kitle", "body_json": tiptap("Bu gönderi gitmemeli."), "commercial_flag": true,
		"audience": map[string]any{"definition": map[string]any{"dept": []string{smallDept.String()}, "year": []int{1}}},
	}, map[string]string{"Idempotency-Key": uuid.NewV7().String()}, http.StatusUnprocessableEntity)
	if tooSmall["code"] != "outreach.audience_too_small" {
		t.Fatalf("an audience below 10: %v", tooSmall)
	}
	tp := c.do(http.MethodPost, "/api/v1/orgs/"+company.String()+"/targeted-posts", owner, map[string]any{
		"kind": "text", "title": "Staj programı", "body_json": tiptap("Yaz stajı başvuruları açıldı."), "commercial_flag": true,
		"audience": map[string]any{"segment_id": segment.String()},
	}, map[string]string{"Idempotency-Key": uuid.NewV7().String()}, http.StatusCreated)
	ent, _ := tp["entitlement"].(map[string]any)
	if est, _ := tp["audience_estimate"].(map[string]any); est == nil || est["below_threshold"] != false || ent == nil || ent["remaining"] != float64(3) {
		t.Fatalf("targeted post: estimate %v, entitlement %v (5 granted, 1 seeded, 1 now)", tp["audience_estimate"], ent)
	}
	tpID := tp["post_id"].(string)
	if p := c.do(http.MethodPost, "/api/v1/posts/"+tpID+"/publish", owner, nil, nil, http.StatusOK); p["status"] != "published" {
		t.Fatalf("publish targeted post: %v", p)
	}
	if c.feedItem(seeded, tpID) == nil {
		t.Fatal("the targeted post is not in the feed of an adult student of the university")
	}
	if c.feedItem(minor, tpID) != nil {
		t.Fatal("a minor sees a commercial targeted post")
	}
	var used int
	q(`SELECT quantity_used FROM entitlements WHERE org_id = $1 AND feature = 'targeted_post' AND source_type = 'admin_grant'`, &used, company)
	if used != 2 {
		t.Fatalf("quota used = %d, want 2", used)
	}

	// --- 5. KVKK: export, then erasure ---
	exp := c.do(http.MethodPost, "/api/v1/me/data-requests", student, map[string]any{"type": "export"}, nil, http.StatusCreated)
	dsr := exp["dsr_id"].(string)
	c.do(http.MethodPost, "/api/v1/admin/data-requests/"+dsr+"/complete", dpo, map[string]any{
		"response_summary": "Verileriniz dışa aktarıldı.", "reauth": map[string]any{"totp_code": dpoTOTP.code()},
	}, nil, http.StatusAccepted)
	var link map[string]any
	waitUntil(t, "export ready", func() bool {
		var st int
		link, st = c.try(http.MethodGet, "/api/v1/me/data-requests/"+dsr+"/export", student)
		return st == http.StatusOK
	})
	zipped := c.fetch(link["url"].(string))
	zr, err := zip.NewReader(bytes.NewReader(zipped), int64(len(zipped)))
	if err != nil {
		t.Fatalf("export is not a zip: %v", err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if !slices.Contains(names, "manifest.json") || !slices.Contains(names, "identity.json") || !slices.Contains(names, "events.json") {
		t.Fatalf("export files = %v", names)
	}
	era := c.do(http.MethodPost, "/api/v1/me/data-requests", student, map[string]any{
		"type": "erasure", "reauth": map[string]any{"password": studentPassword},
	}, nil, http.StatusCreated)
	c.do(http.MethodPost, "/api/v1/admin/data-requests/"+era["dsr_id"].(string)+"/complete", dpo, map[string]any{
		"response_summary": "Hesabınız silindi.", "reauth": map[string]any{"totp_code": dpoTOTP.code()},
	}, nil, http.StatusAccepted)
	waitUntil(t, "account erased", func() bool {
		var status string
		err := d.Pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, uuid.MustParse(me["user_id"].(string))).Scan(&status)
		return err == nil && status == "erased"
	})
	if st := c.status(http.MethodGet, "/api/v1/me", student); st != http.StatusUnauthorized {
		t.Fatalf("GET /me with the erased account's token = %d, want 401", st)
	}
	_, st := c.try(http.MethodPost, "/api/v1/auth/sessions", "", map[string]any{"email": studentEmail, "password": studentPassword})
	if st != http.StatusUnauthorized {
		t.Fatalf("sign-in of the erased account = %d, want 401", st)
	}
	took := time.Since(start)
	t.Logf("smoke flow took %s (containers ready beforehand)", took.Round(time.Second))
	if took > 3*time.Minute {
		t.Errorf("smoke test took %s, want ≤ 3 minutes", took.Round(time.Second))
	}
}

// startProcess runs a koro process in a goroutine until the test ends and
// returns the field key of its ready log line (msg ready).
func startProcess(t *testing.T, ctx context.Context, ready, key string,
	run func(context.Context, context.CancelFunc, io.Writer) error) string {
	t.Helper()
	pctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(pctx, stop, pw)
		_ = pw.Close()
	}()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s: %v", ready, err)
			}
		case <-time.After(30 * time.Second):
			t.Errorf("%s: did not stop", ready)
		}
	})
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	val := ""
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) == nil && rec["msg"] == ready {
			if key != "" {
				val, _ = rec[key].(string)
			}
			go func() { _, _ = io.Copy(io.Discard, pr) }()
			return val
		}
	}
	t.Fatalf("%s: the process stopped before it was ready", ready)
	return ""
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not reached in 60s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// tiptap is a one-paragraph post body.
func tiptap(text string) map[string]any {
	return map[string]any{"type": "doc", "content": []any{map[string]any{"type": "paragraph",
		"content": []any{map[string]any{"type": "text", "text": text}}}}}
}

// smokeClient calls the API with bearer tokens (mobile clients).
type smokeClient struct {
	t    *testing.T
	ctx  context.Context
	base string
	http *http.Client
}

func (c *smokeClient) send(method, path, token string, body any, headers map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Koro-Client", "mobile")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// doH calls the API, requires the status and returns the JSON body and
// the headers.
func (c *smokeClient) doH(method, path, token string, body any, headers map[string]string, want int) (map[string]any, http.Header) {
	c.t.Helper()
	resp, raw := c.send(method, path, token, body, headers)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
	return out, resp.Header
}

func (c *smokeClient) do(method, path, token string, body any, headers map[string]string, want int) map[string]any {
	c.t.Helper()
	out, _ := c.doH(method, path, token, body, headers, want)
	return out
}

// try calls the API and returns the JSON body and the status.
func (c *smokeClient) try(method, path, token string, body ...any) (map[string]any, int) {
	c.t.Helper()
	var b any
	if len(body) > 0 {
		b = body[0]
	}
	resp, raw := c.send(method, path, token, b, nil)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out, resp.StatusCode
}

func (c *smokeClient) status(method, path string, token ...string) int {
	c.t.Helper()
	tok := ""
	if len(token) > 0 {
		tok = token[0]
	}
	_, st := c.try(method, path, tok)
	return st
}

// login signs in as a mobile client and returns the bearer token.
func (c *smokeClient) login(email, password string) string {
	c.t.Helper()
	out := c.do(http.MethodPost, "/api/v1/auth/sessions", "", map[string]any{"email": email, "password": password}, nil, http.StatusOK)
	tok, _ := out["token"].(string)
	if tok == "" {
		c.t.Fatalf("sign-in of %s: no token: %v", email, out)
	}
	return tok
}

// feedItem returns the item of postID in the reader's feed (first page).
func (c *smokeClient) feedItem(token, postID string) map[string]any {
	c.t.Helper()
	feed := c.do(http.MethodGet, "/api/v1/feed?limit=50", token, nil, nil, http.StatusOK)
	items, _ := feed["items"].([]any)
	for _, it := range items {
		if m := it.(map[string]any); m["post_id"] == postID {
			return m
		}
	}
	return nil
}

// fetch downloads a presigned URL.
func (c *smokeClient) fetch(u string) []byte {
	c.t.Helper()
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, u, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("GET %s = %d: %s", u, resp.StatusCode, b)
	}
	return b
}

// enrolTOTP turns MFA on for the session's account (enrolTotp +
// confirmTotp: the session becomes MFA-verified) and returns the
// authenticator for later re-authentications.
func (c *smokeClient) enrolTOTP(token, password string) *authenticator {
	c.t.Helper()
	enr := c.do(http.MethodPost, "/api/v1/me/mfa/totp", token, map[string]any{"reauth": map[string]any{"password": password}},
		nil, http.StatusOK)
	a := &authenticator{t: c.t, secret: enr["secret_base32"].(string)}
	c.do(http.MethodPost, "/api/v1/me/mfa/totp/confirm", token, map[string]any{"code": a.code(),
		"reauth": map[string]any{"password": password}}, nil, http.StatusOK)
	return a
}

// authenticator computes TOTP codes (RFC 6238: SHA1, 6 digits, 30 s). The
// server refuses a step it accepted before, so every code is of a new
// step: the current one, or the next (one step of tolerance); a third code
// within one step waits for the clock.
type authenticator struct {
	t      *testing.T
	mu     sync.Mutex
	secret string
	last   int64
}

func (a *authenticator) code() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	step := max(time.Now().Unix()/30, a.last+1)
	for step > time.Now().Unix()/30+1 {
		time.Sleep(time.Second)
	}
	a.last = step
	c, err := totp.GenerateCodeCustom(a.secret, time.Unix(step*30, 0).UTC(),
		totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		a.t.Fatal(err)
	}
	return c
}

// mailpit reads the messages of the test's Mailpit.
type mailpit struct {
	t   *testing.T
	ctx context.Context
	api string
}

type mailpitMsg struct {
	ID          string
	Subject     string
	Text        string
	HTML        string
	Inline      []mailpitAttachment
	Attachments []mailpitAttachment
}

type mailpitAttachment struct {
	ContentID   string
	ContentType string
	FileName    string
}

func (m *mailpit) get(path string, into any) {
	m.t.Helper()
	req, err := http.NewRequestWithContext(m.ctx, http.MethodGet, m.api+path, nil)
	if err != nil {
		m.t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		m.t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		m.t.Fatalf("mailpit %s: %v", path, err)
	}
}

// wait returns the first message to `to` whose subject starts with prefix.
func (m *mailpit) wait(to, prefix string) mailpitMsg {
	m.t.Helper()
	var found mailpitMsg
	waitUntil(m.t, "e-mail to "+to, func() bool {
		var list struct {
			Messages []struct{ ID, Subject string }
		}
		m.get("/api/v1/search?query="+url.QueryEscape(`to:"`+to+`"`), &list)
		for _, msg := range list.Messages {
			if strings.HasPrefix(msg.Subject, prefix) {
				m.get("/api/v1/message/"+msg.ID, &found)
				return true
			}
		}
		return false
	})
	return found
}

var linkToken = regexp.MustCompile(`#token=([A-Za-z0-9_\-]+)`)

// token returns the token of the newest link e-mail to `to`.
func (m *mailpit) token(to string) string {
	m.t.Helper()
	msg := m.wait(to, "")
	match := linkToken.FindStringSubmatch(msg.Text)
	if match == nil {
		m.t.Fatalf("no link token in the e-mail to %s: %q", to, msg.Subject)
	}
	return match[1]
}

// startMailpit runs Mailpit (R-002: v1.28) for the test.
func startMailpit(t *testing.T) (host string, smtpPort int, api string) {
	t.Helper()
	var ctr *testcontainers.DockerContainer
	err := testutil.Retry(t.Context(), testutil.Backoff{Attempts: 3, Base: 2 * time.Second, Max: 10 * time.Second}, "start mailpit", nil,
		func(ctx context.Context) error {
			c, err := testcontainers.Run(ctx, "axllent/mailpit:v1.28",
				testcontainers.WithExposedPorts("1025/tcp", "8025/tcp"),
				testcontainers.WithWaitStrategy(wait.ForHTTP("/readyz").WithPort("8025/tcp").WithStartupTimeout(2*time.Minute)),
			)
			if err != nil {
				if c != nil {
					_ = c.Terminate(context.WithoutCancel(ctx))
				}
				return err
			}
			ctr = c
			return nil
		})
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
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(fmt.Errorf("mailpit port: %w", err))
	}
	return h, port, web
}
