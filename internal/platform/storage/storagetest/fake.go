// Package storagetest helps testing storage.Objects adapters: FakeS3 is an
// in-process S3 endpoint for unit tests, and with the integration build tag
// Main/Start/Client/Bucket run a SeaweedFS container (storagetest.go).
package storagetest

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// FakeS3 is an in-process, path-style S3 endpoint for unit tests (BE-113):
// enough of the S3 REST API for storage.S3 — PUT (plain, presigned and
// copy), HEAD, GET with response-* overrides, DELETE and bucket creation —
// with the error codes the adapter maps (NoSuchKey, a bodiless 404 on HEAD).
// It does not verify signatures or expiry; the SeaweedFS container of this
// package (-tags integration) does. Every bucket exists.
type FakeS3 struct {
	// URL is the endpoint (S3_ENDPOINT) of the fake.
	URL string

	srv     *httptest.Server
	mu      sync.Mutex
	objects map[string]fakeObject // "<bucket>/<key>"
}

type fakeObject struct {
	data                     []byte
	contentType, disposition string
}

// NewFakeS3 starts the fake; it stops when the test ends.
func NewFakeS3(t testing.TB) *FakeS3 {
	t.Helper()
	f := &FakeS3{objects: map[string]fakeObject{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	f.URL = f.srv.URL
	t.Cleanup(f.srv.Close)
	return f
}

// Client is an HTTP client for the fake (browser-side requests of tests).
func (f *FakeS3) Client() *http.Client { return f.srv.Client() }

// Len returns the number of stored objects.
func (f *FakeS3) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objects)
}

func (f *FakeS3) serve(w http.ResponseWriter, r *http.Request) {
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket == "" {
		s3Error(w, http.StatusBadRequest, "InvalidBucketName")
		return
	}
	if key == "" { // bucket operations: creation only
		if r.Method == http.MethodPut {
			w.WriteHeader(http.StatusOK)
			return
		}
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
		return
	}
	id := bucket + "/" + key
	switch r.Method {
	case http.MethodPut:
		if src := r.Header.Get("X-Amz-Copy-Source"); src != "" {
			f.copy(w, r, id, src)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		f.mu.Lock()
		f.objects[id] = fakeObject{data: body, contentType: r.Header.Get("Content-Type"), disposition: r.Header.Get("Content-Disposition")}
		f.mu.Unlock()
		w.Header().Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodHead, http.MethodGet:
		f.mu.Lock()
		o, ok := f.objects[id]
		f.mu.Unlock()
		if !ok {
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusNotFound) // S3 answers HEAD without a body
				return
			}
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		h := w.Header()
		h.Set("Content-Type", o.contentType)
		if o.disposition != "" {
			h.Set("Content-Disposition", o.disposition)
		}
		q := r.URL.Query()
		if v := q.Get("response-content-type"); v != "" {
			h.Set("Content-Type", v)
		}
		if v := q.Get("response-content-disposition"); v != "" {
			h.Set("Content-Disposition", v)
		}
		h.Set("Content-Length", strconv.Itoa(len(o.data)))
		h.Set("ETag", `"fake"`)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(o.data)
		}
	case http.MethodDelete:
		f.mu.Lock()
		delete(f.objects, id)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// copy implements CopyObject with MetadataDirective REPLACE.
func (f *FakeS3) copy(w http.ResponseWriter, r *http.Request, dst, src string) {
	if s, err := url.PathUnescape(src); err == nil {
		src = s
	}
	src = strings.TrimPrefix(src, "/")
	f.mu.Lock()
	o, ok := f.objects[src]
	if ok {
		f.objects[dst] = fakeObject{data: bytes.Clone(o.data), contentType: r.Header.Get("Content-Type"),
			disposition: r.Header.Get("Content-Disposition")}
	}
	f.mu.Unlock()
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header+`<CopyObjectResult><ETag>"fake"</ETag>`+
		`<LastModified>2026-10-03T09:00:00.000Z</LastModified></CopyObjectResult>`)
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header+"<Error><Code>"+code+"</Code><Message>"+code+"</Message></Error>")
}
