//go:build integration

package storage_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"koro/internal/platform/storage"
	"koro/internal/platform/storage/storagetest"
)

func TestMain(m *testing.M) { os.Exit(storagetest.Main(m, (*testing.M).Run)) }

var httpClient = &http.Client{Timeout: 30 * time.Second}

// browserPut sends what a browser sends with a presigned PUT: the given
// body with the returned headers (Content-Length set by the client).
func browserPut(t *testing.T, p storage.Presigned, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), p.Method, p.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

// TestS3_SeaweedFS_PresignedRoundTrip runs the upload/download contract of
// 05 §14.7 against SeaweedFS (R-002 §3i: WhenRequired checksums, presigned
// URLs; T-05).
func TestS3_SeaweedFS_PresignedRoundTrip(t *testing.T) {
	t.Parallel()
	s3 := storagetest.Client(t)
	quarantine, files := storagetest.Bucket(t, s3), storagetest.Bucket(t, s3)
	ctx := t.Context()
	content := []byte("%PDF-1.7\n% storage integration\n%%EOF\n")
	key := "cv/2026/10/0199a1b2-0000-7000-8000-000000000001"

	put, err := s3.PresignPut(ctx, storage.PresignPutInput{
		Bucket: quarantine, Key: key, ContentType: "application/pdf", ContentLength: int64(len(content)), TTL: 10 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Another size or type breaks the signature (signed headers).
	if resp := browserPut(t, put, append(bytes.Clone(content), 'x')); resp.StatusCode < 400 {
		t.Errorf("PUT with another length: status %d, want a signature error", resp.StatusCode)
	}
	wrongType := put
	wrongType.Headers = map[string]string{"Content-Type": "text/html"}
	if resp := browserPut(t, wrongType, content); resp.StatusCode < 400 {
		t.Errorf("PUT with another content type: status %d, want a signature error", resp.StatusCode)
	}
	if _, err := s3.Stat(ctx, quarantine, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("a rejected PUT stored the object: %v", err)
	}

	if resp := browserPut(t, put, content); resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned PUT: status %d", resp.StatusCode)
	}
	info, err := s3.Stat(ctx, quarantine, key)
	if err != nil || info.Size != int64(len(content)) {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	rc, _, err := s3.Open(ctx, quarantine, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("Open = %q", got)
	}

	// Server-side move out of quarantine with new metadata.
	if err := s3.Copy(ctx, storage.CopyInput{SrcBucket: quarantine, SrcKey: key, DstBucket: files, DstKey: key,
		ContentType: "application/pdf", ContentDisposition: "attachment"}); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := s3.Delete(ctx, quarantine, key); err != nil {
		t.Fatal(err)
	}
	if err := s3.Delete(ctx, quarantine, key); err != nil {
		t.Fatalf("Delete of a missing object: %v", err)
	}

	get, err := s3.PresignGet(ctx, storage.PresignGetInput{Bucket: files, Key: key, TTL: storage.MaxGetTTL,
		ResponseContentDisposition: `attachment; filename=cv.pdf`, ResponseContentType: "application/pdf"})
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, get.URL, nil)
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
		t.Fatalf("presigned GET: status %d, %d bytes", resp.StatusCode, len(body))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("Content-Type = %q", ct)
	}

	// The bucket is private: an unsigned request is refused.
	anon, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.Split(get.URL, "?")[0], nil)
	resp, err = httpClient.Do(anon)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("anonymous GET: status %d, want 403", resp.StatusCode)
	}
}

func TestS3_SeaweedFS_PutObjectWithoutChecksumTrailer(t *testing.T) {
	t.Parallel()
	s3 := storagetest.Client(t)
	bucket := storagetest.Bucket(t, s3)
	ctx := t.Context()
	body := bytes.Repeat([]byte("ad;soyad\n"), 1000)
	if err := s3.PutObject(ctx, storage.PutInput{Bucket: bucket, Key: "event_export/2026/10/x", Body: bytes.NewReader(body),
		Size: int64(len(body)), ContentType: "text/csv", ContentDisposition: "attachment; filename=x.csv"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	rc, info, err := s3.Open(ctx, bucket, "event_export/2026/10/x")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, body) || info.ContentType != "text/csv" {
		t.Fatalf("stored object differs: %d bytes, type %q", len(got), info.ContentType)
	}
	if _, err := s3.Stat(ctx, bucket, "nope"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Stat(missing) = %v", err)
	}
}
