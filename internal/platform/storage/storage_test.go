package storage_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/storage"
)

var t0 = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)

func newS3(t *testing.T, endpoint string) *storage.S3 {
	t.Helper()
	s, err := storage.NewS3(storage.Config{
		Endpoint: endpoint, Region: "us-east-1", AccessKey: "test-access", SecretKey: "test-secret-key",
		PathStyle: true, Clock: clock.NewFake(t0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// implementations returns both Objects implementations (the presign rules
// are the same for both).
func implementations(t *testing.T) map[string]storage.Objects {
	t.Helper()
	return map[string]storage.Objects{
		"s3":  newS3(t, "http://127.0.0.1:1"),
		"mem": storage.NewMem(clock.NewFake(t0)),
	}
}

func getInput(ttl time.Duration) storage.PresignGetInput {
	return storage.PresignGetInput{
		Bucket: "files", Key: "cv/2026/10/x", TTL: ttl,
		ResponseContentDisposition: `attachment; filename=cv.pdf`, ResponseContentType: "application/pdf",
	}
}

func TestStorage_PresignTTLMax5Min(t *testing.T) {
	t.Parallel()
	for name, s := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := s.PresignGet(t.Context(), getInput(storage.MaxGetTTL))
			if err != nil {
				t.Fatalf("PresignGet(5m): %v", err)
			}
			if storage.MaxGetTTL != 5*time.Minute || !p.ExpiresAt.Equal(t0.Add(5*time.Minute)) || p.Method != http.MethodGet {
				t.Fatalf("presigned = %+v", p)
			}
			u, err := url.Parse(p.URL)
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if q.Get("X-Amz-Expires") != "300" {
				t.Fatalf("X-Amz-Expires = %q, want 300", q.Get("X-Amz-Expires"))
			}
			if q.Get("response-content-disposition") != `attachment; filename=cv.pdf` || q.Get("response-content-type") != "application/pdf" {
				t.Fatalf("response overrides missing: %s", p.URL)
			}
			for _, ttl := range []time.Duration{storage.MaxGetTTL + time.Second, time.Hour, 0, -time.Minute} {
				if _, err := s.PresignGet(t.Context(), getInput(ttl)); !errors.Is(err, storage.ErrInvalidTTL) {
					t.Errorf("PresignGet(ttl %s) error = %v, want ErrInvalidTTL", ttl, err)
				}
			}
			in := getInput(time.Minute)
			in.ResponseContentDisposition = ""
			if _, err := s.PresignGet(t.Context(), in); !errors.Is(err, storage.ErrInvalidInput) {
				t.Errorf("PresignGet without a content disposition: %v", err)
			}
		})
	}
}

func TestStorage_PresignPutSignsTypeAndLength(t *testing.T) {
	t.Parallel()
	s := newS3(t, "http://127.0.0.1:1")
	in := storage.PresignPutInput{Bucket: "quarantine", Key: "cv/2026/10/x", ContentType: "application/pdf", ContentLength: 1234, TTL: 10 * time.Minute}
	p, err := s.PresignPut(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(p.URL)
	q := u.Query()
	if got := q.Get("X-Amz-SignedHeaders"); got != "content-length;content-type;host" {
		t.Fatalf("signed headers %q: type and length must be signed", got)
	}
	if q.Get("X-Amz-Expires") != "600" || p.Method != http.MethodPut || u.Path != "/quarantine/cv/2026/10/x" {
		t.Fatalf("presigned PUT %s", p.URL)
	}
	if len(p.Headers) != 1 || p.Headers["Content-Type"] != "application/pdf" {
		t.Fatalf("client headers %v, want only Content-Type (browsers set Content-Length)", p.Headers)
	}
	for k := range q {
		if strings.Contains(strings.ToLower(k), "checksum") {
			t.Fatalf("presigned PUT carries a checksum parameter %s (R-002 §3i)", k)
		}
	}
	in.TTL = storage.MaxPutTTL + time.Second
	if _, err := s.PresignPut(t.Context(), in); !errors.Is(err, storage.ErrInvalidTTL) {
		t.Fatalf("PresignPut over 10m: %v", err)
	}
	in.TTL, in.ContentLength = time.Minute, 0
	if _, err := s.PresignPut(t.Context(), in); !errors.Is(err, storage.ErrInvalidInput) {
		t.Fatalf("PresignPut without a length: %v", err)
	}
}

func TestStorage_PresignEndpointIsSignedForBrowsers(t *testing.T) {
	t.Parallel()
	s, err := storage.NewS3(storage.Config{
		Endpoint: "http://seaweedfs:8333", PresignEndpoint: "https://files.koro.example", Region: "us-east-1",
		AccessKey: "a", SecretKey: "b", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.PresignGet(t.Context(), getInput(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(p.URL, "https://files.koro.example/files/") {
		t.Fatalf("URL %s is not on the public endpoint", p.URL)
	}
	if _, err := storage.NewS3(storage.Config{Endpoint: "http://x"}); !errors.Is(err, storage.ErrInvalidInput) {
		t.Fatalf("NewS3 without credentials: %v", err)
	}
}

// recorder is a fake S3 endpoint that records request headers.
type recorder struct {
	mu   sync.Mutex
	reqs []*http.Request
	body [][]byte
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	r.reqs = append(r.reqs, req.Clone(req.Context()))
	r.body = append(r.body, b)
	r.mu.Unlock()
	switch {
	case req.Method == http.MethodHead && strings.HasSuffix(req.URL.Path, "/missing"):
		w.WriteHeader(http.StatusNotFound)
	case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/missing"):
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>no</Message></Error>`)
	case req.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case req.Method == http.MethodPut && req.Header.Get("X-Amz-Copy-Source") != "":
		_, _ = io.WriteString(w, `<CopyObjectResult><ETag>"x"</ETag></CopyObjectResult>`)
	default:
		w.Header().Set("ETag", `"x"`)
		w.WriteHeader(http.StatusOK)
	}
}

func TestStorage_ServerCallsCarryNoDefaultChecksum(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	s := newS3(t, srv.URL)
	ctx := t.Context()

	body := []byte("%PDF-1.7 generated")
	if err := s.PutObject(ctx, storage.PutInput{Bucket: "files", Key: "event_export/2026/10/x", Body: bytes.NewReader(body),
		Size: int64(len(body)), ContentType: "text/csv", ContentDisposition: "attachment; filename=x.csv"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if err := s.Copy(ctx, storage.CopyInput{SrcBucket: "q", SrcKey: "cv/2026/10/x", DstBucket: "files", DstKey: "cv/2026/10/x",
		ContentType: "application/pdf", ContentDisposition: "attachment"}); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if _, err := s.Stat(ctx, "files", "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat(missing) = %v, want ErrNotFound", err)
	}
	if _, _, err := s.Open(ctx, "files", "missing"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Open(missing) = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "files", "missing"); err != nil {
		t.Fatalf("Delete(missing) = %v, want nil", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	put := rec.reqs[0]
	for k := range put.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-checksum") || strings.EqualFold(k, "X-Amz-Sdk-Checksum-Algorithm") ||
			strings.EqualFold(k, "X-Amz-Trailer") {
			t.Fatalf("PutObject sent %s: RequestChecksumCalculation must be WhenRequired (R-002 §3i)", k)
		}
	}
	if put.Header.Get("Content-Encoding") == "aws-chunked" || !bytes.Equal(rec.body[0], body) {
		t.Fatalf("PutObject body is not sent as is (encoding %q)", put.Header.Get("Content-Encoding"))
	}
	if put.Header.Get("Content-Disposition") != "attachment; filename=x.csv" || put.Header.Get("Content-Type") != "text/csv" {
		t.Fatalf("PutObject metadata headers %v", put.Header)
	}
	cp := rec.reqs[1]
	if cp.Header.Get("X-Amz-Copy-Source") != "q/cv/2026/10/x" || cp.Header.Get("X-Amz-Metadata-Directive") != "REPLACE" ||
		cp.Header.Get("Content-Disposition") != "attachment" {
		t.Fatalf("Copy headers %v", cp.Header)
	}
}

func TestMem_BehavesLikeS3(t *testing.T) {
	t.Parallel()
	m := storage.NewMem(clock.NewFake(t0))
	ctx := t.Context()
	if _, err := m.Stat(ctx, "q", "k"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat(missing) = %v", err)
	}
	m.Upload("q", "k", []byte("hello"), "text/plain")
	if info, err := m.Stat(ctx, "q", "k"); err != nil || info.Size != 5 {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	if err := m.Copy(ctx, storage.CopyInput{SrcBucket: "q", SrcKey: "k", DstBucket: "f", DstKey: "k", ContentType: "application/pdf", ContentDisposition: "attachment"}); err != nil {
		t.Fatal(err)
	}
	rc, _, err := m.Open(ctx, "f", "k")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(b) != "hello" {
		t.Fatalf("copy = %q", b)
	}
	if err := m.Delete(ctx, "q", "k"); err != nil || m.Len() != 1 {
		t.Fatalf("Delete: %v, %d objects", err, m.Len())
	}
	if err := m.PutObject(ctx, storage.PutInput{Bucket: "f", Key: "x", Body: strings.NewReader("abc"), Size: 4, ContentType: "text/csv"}); !errors.Is(err, storage.ErrInvalidInput) {
		t.Fatalf("PutObject with a wrong size: %v", err)
	}
	m.FailNext(errors.New("down"))
	if err := m.Delete(ctx, "f", "k"); err == nil {
		t.Fatal("FailNext ignored")
	}
}
