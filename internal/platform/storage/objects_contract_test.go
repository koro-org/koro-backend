package storage_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"koro/internal/platform/clock"
	"koro/internal/platform/storage"
	"koro/internal/platform/storage/storagetest"
)

// objectsFixture is what the Objects contract suite needs from the
// adapter's environment.
type objectsFixture struct {
	// Quarantine and Files are two empty, private buckets.
	Quarantine, Files string
	// Now reads the adapter's clock (presign expiry).
	Now func() time.Time
	// ClientPut performs a presigned upload as a browser does (the
	// returned headers, Content-Length set by the client).
	ClientPut func(t *testing.T, p storage.Presigned, body []byte)
}

// objectsContract is the contract suite of storage.Objects (BE-113): Mem and
// S3 pass it — S3 against storagetest.FakeS3 here and SeaweedFS with
// -tags integration. A new storage adapter is a new file in this package,
// one line in cmd/koro/wire_files.go objectStores and a
// Test<Adapter>_ContractSuite that runs this suite. The steps run in order
// and share the buckets.
func objectsContract(t *testing.T, s storage.Objects, fx objectsFixture) {
	t.Helper()
	ctx := t.Context()
	q, f := fx.Quarantine, fx.Files
	content := []byte("%PDF-1.7 contract suite object")
	// expires checks a presign expiry against the clock around the call.
	expires := func(got, before, after time.Time, ttl time.Duration) bool {
		return !got.Before(before.Add(ttl)) && !got.After(after.Add(ttl))
	}

	t.Run("a missing object is ErrNotFound", func(t *testing.T) {
		if _, err := s.Stat(ctx, q, "missing/key"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Stat(missing) = %v", err)
		}
		if _, _, err := s.Open(ctx, q, "missing/key"); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Open(missing) = %v", err)
		}
		err := s.Copy(ctx, storage.CopyInput{SrcBucket: q, SrcKey: "missing/key", DstBucket: f, DstKey: "x", ContentType: "text/plain"})
		if !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Copy(missing) = %v", err)
		}
	})

	t.Run("a server-side put is stored as given", func(t *testing.T) {
		err := s.PutObject(ctx, storage.PutInput{Bucket: f, Key: "exports/a.csv", Body: bytes.NewReader([]byte("a,b\n")),
			Size: 4, ContentType: "text/csv", ContentDisposition: "attachment; filename=a.csv"})
		if err != nil {
			t.Fatal(err)
		}
		rc, info, err := s.Open(ctx, f, "exports/a.csv")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(b) != "a,b\n" || info.Size != 4 || info.ContentType != "text/csv" {
			t.Fatalf("Open = %q, %+v", b, info)
		}
	})

	key := "cv/2026/10/contract"
	t.Run("a presigned upload signs type and length and stores the object", func(t *testing.T) {
		before := fx.Now()
		p, err := s.PresignPut(ctx, storage.PresignPutInput{Bucket: q, Key: key, ContentType: "application/pdf",
			ContentLength: int64(len(content)), TTL: storage.MaxPutTTL})
		if err != nil {
			t.Fatal(err)
		}
		if p.Method != http.MethodPut || p.URL == "" || !expires(p.ExpiresAt, before, fx.Now(), storage.MaxPutTTL) ||
			p.Headers["Content-Type"] != "application/pdf" {
			t.Fatalf("PresignPut = %+v", p)
		}
		if _, ok := p.Headers["Host"]; ok {
			t.Fatalf("headers %v name Host: browsers cannot set it", p.Headers)
		}
		fx.ClientPut(t, p, content)
		info, err := s.Stat(ctx, q, key)
		if err != nil || info.Size != int64(len(content)) || info.ContentType != "application/pdf" {
			t.Fatalf("Stat after upload = %+v, %v", info, err)
		}
	})

	t.Run("a presigned download is short-lived and overrides the response headers", func(t *testing.T) {
		in := storage.PresignGetInput{Bucket: q, Key: key, TTL: storage.MaxGetTTL,
			ResponseContentDisposition: "attachment; filename=cv.pdf", ResponseContentType: "application/pdf"}
		before := fx.Now()
		p, err := s.PresignGet(ctx, in)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(p.URL)
		if err != nil || p.Method != http.MethodGet || !expires(p.ExpiresAt, before, fx.Now(), storage.MaxGetTTL) ||
			u.Query().Get("response-content-disposition") != in.ResponseContentDisposition {
			t.Fatalf("PresignGet = %+v (%v)", p, err)
		}
		in.TTL = storage.MaxGetTTL + time.Second
		if _, err := s.PresignGet(ctx, in); !errors.Is(err, storage.ErrInvalidTTL) {
			t.Fatalf("PresignGet(ttl > 5m) = %v, want ErrInvalidTTL", err)
		}
		in.TTL, in.ResponseContentDisposition = time.Minute, ""
		if _, err := s.PresignGet(ctx, in); !errors.Is(err, storage.ErrInvalidInput) {
			t.Fatalf("PresignGet(no disposition) = %v, want ErrInvalidInput", err)
		}
	})

	t.Run("copy replaces the metadata and keeps the source", func(t *testing.T) {
		err := s.Copy(ctx, storage.CopyInput{SrcBucket: q, SrcKey: key, DstBucket: f, DstKey: key,
			ContentType: "application/pdf", ContentDisposition: "attachment"})
		if err != nil {
			t.Fatal(err)
		}
		rc, info, err := s.Open(ctx, f, key)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(b, content) || info.ContentType != "application/pdf" {
			t.Fatalf("copy = %q, %+v", b, info)
		}
		if _, err := s.Stat(ctx, q, key); err != nil {
			t.Fatalf("source after copy: %v", err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		for range 2 {
			if err := s.Delete(ctx, q, key); err != nil {
				t.Fatalf("Delete = %v", err)
			}
		}
		if _, err := s.Stat(ctx, q, key); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Stat after delete = %v", err)
		}
	})

	t.Run("invalid input is refused before any request", func(t *testing.T) {
		if err := s.PutObject(ctx, storage.PutInput{Bucket: f, Key: "k", Body: strings.NewReader("x"), Size: 1}); !errors.Is(err, storage.ErrInvalidInput) {
			t.Fatalf("PutObject(no content type) = %v", err)
		}
		_, err := s.PresignPut(ctx, storage.PresignPutInput{Bucket: q, Key: "k", ContentType: "image/png", ContentLength: 1,
			TTL: storage.MaxPutTTL + time.Second})
		if !errors.Is(err, storage.ErrInvalidTTL) {
			t.Fatalf("PresignPut(ttl > max) = %v", err)
		}
		if err := s.Copy(ctx, storage.CopyInput{SrcBucket: q, SrcKey: "k", DstBucket: f, DstKey: "k"}); !errors.Is(err, storage.ErrInvalidInput) {
			t.Fatalf("Copy(no content type) = %v", err)
		}
	})

	t.Run("concurrent puts are independent", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Go(func() {
				body := fmt.Sprintf("object %d", i)
				errs[i] = s.PutObject(ctx, storage.PutInput{Bucket: f, Key: fmt.Sprintf("par/%d", i),
					Body: strings.NewReader(body), Size: int64(len(body)), ContentType: "text/plain"})
			})
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			t.Fatal(err)
		}
		for i := range errs {
			if info, err := s.Stat(ctx, f, fmt.Sprintf("par/%d", i)); err != nil || info.Size != int64(len(fmt.Sprintf("object %d", i))) {
				t.Fatalf("par/%d = %+v, %v", i, info, err)
			}
		}
	})
}

func TestMem_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	m := storage.NewMem(clock.NewFake(t0))
	objectsContract(t, m, objectsFixture{Quarantine: "koro-quarantine-test", Files: "koro-files-test", Now: func() time.Time { return t0 },
		ClientPut: func(t *testing.T, p storage.Presigned, body []byte) {
			t.Helper()
			u, err := url.Parse(p.URL)
			if err != nil {
				t.Fatal(err)
			}
			bucket, key, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
			m.Upload(bucket, key, body, p.Headers["Content-Type"])
		}})
}

func TestS3_ContractSuite(t *testing.T) { //nolint:tparallel // the contract's steps share the adapter's state and run in order
	t.Parallel()
	fake := storagetest.NewFakeS3(t)
	objectsContract(t, newS3(t, fake.URL), objectsFixture{Quarantine: "koro-quarantine-test", Files: "koro-files-test", Now: func() time.Time { return t0 },
		ClientPut: func(t *testing.T, p storage.Presigned, body []byte) {
			t.Helper()
			clientPut(t, fake.Client(), p, body)
		}})
}

// clientPut sends what a browser sends with a presigned PUT.
func clientPut(t *testing.T, c *http.Client, p storage.Presigned, body []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), p.Method, p.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT = %d", resp.StatusCode)
	}
}
