package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"koro/internal/platform/clock"
)

// Mem is the in-memory Objects of service, wiring and matrix tests. It
// validates its inputs exactly like S3 and returns presigned URLs on the
// reserved host mem-storage.invalid. Upload simulates the client's PUT.
type Mem struct {
	mu      sync.Mutex
	clock   clock.Clock
	objects map[string]memObject // "<bucket>/<key>"
	// failNext is returned once by the next Copy, Delete or Open call
	// (FailNext; tests of failure paths).
	failNext error
}

type memObject struct {
	data               []byte
	contentType        string
	contentDisposition string
	cacheControl       string
}

func (o memObject) info() ObjectInfo {
	return ObjectInfo{Size: int64(len(o.data)), ContentType: o.contentType, CacheControl: o.cacheControl}
}

var _ Objects = (*Mem)(nil)

// NewMem returns an empty store; clk dates the presigned URLs.
func NewMem(clk clock.Clock) *Mem {
	return &Mem{clock: clk, objects: map[string]memObject{}}
}

func memKey(bucket, key string) string { return bucket + "/" + key }

// Upload stores an object as the client's presigned PUT would.
func (m *Mem) Upload(bucket, key string, data []byte, contentType string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[memKey(bucket, key)] = memObject{data: bytes.Clone(data), contentType: contentType}
}

// Object returns a stored object and its metadata.
func (m *Mem) Object(bucket, key string) (data []byte, contentType, contentDisposition string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[memKey(bucket, key)]
	return bytes.Clone(o.data), o.contentType, o.contentDisposition, ok
}

// Len returns the number of stored objects.
func (m *Mem) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// FailNext makes the next Copy, Delete or Open call return err.
func (m *Mem) FailNext(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext = err
}

func (m *Mem) takeFailure() error {
	err := m.failNext
	m.failNext = nil
	return err
}

// PutObject implements Objects.
func (m *Mem) PutObject(_ context.Context, in PutInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(in.Body, in.Size+1))
	if err != nil {
		return fmt.Errorf("storage: put object: %w", err)
	}
	if int64(len(data)) != in.Size {
		return fmt.Errorf("%w: body has %d bytes, size says %d", ErrInvalidInput, len(data), in.Size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[memKey(in.Bucket, in.Key)] = memObject{data: data, contentType: in.ContentType, contentDisposition: in.ContentDisposition}
	return nil
}

func (m *Mem) presigned(method, bucket, key string, q url.Values, signed http.Header, ttl time.Duration) Presigned {
	now := m.clock.Now()
	q.Set("X-Amz-Expires", strconv.FormatInt(int64(ttl.Seconds()), 10))
	u := url.URL{Scheme: "https", Host: "mem-storage.invalid", Path: "/" + bucket + "/" + key, RawQuery: q.Encode()}
	return Presigned{URL: u.String(), Method: method, Headers: clientHeaders(signed), ExpiresAt: now.Add(ttl)}
}

// PresignPut implements Objects.
func (m *Mem) PresignPut(_ context.Context, in PresignPutInput) (Presigned, error) {
	if err := in.validate(); err != nil {
		return Presigned{}, err
	}
	h := http.Header{}
	h.Set("Content-Type", in.ContentType)
	h.Set("Content-Length", strconv.FormatInt(in.ContentLength, 10))
	return m.presigned(http.MethodPut, in.Bucket, in.Key, url.Values{}, h, in.TTL), nil
}

// PresignGet implements Objects.
func (m *Mem) PresignGet(_ context.Context, in PresignGetInput) (Presigned, error) {
	if err := in.validate(); err != nil {
		return Presigned{}, err
	}
	q := url.Values{}
	q.Set("response-content-disposition", in.ResponseContentDisposition)
	q.Set("response-content-type", in.ResponseContentType)
	return m.presigned(http.MethodGet, in.Bucket, in.Key, q, http.Header{}, in.TTL), nil
}

// Stat implements Objects.
func (m *Mem) Stat(_ context.Context, bucket, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[memKey(bucket, key)]
	if !ok {
		return ObjectInfo{}, fmt.Errorf("storage: stat object: %w", ErrNotFound)
	}
	return o.info(), nil
}

// Open implements Objects.
func (m *Mem) Open(_ context.Context, bucket, key string) (io.ReadCloser, ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takeFailure(); err != nil {
		return nil, ObjectInfo{}, err
	}
	o, ok := m.objects[memKey(bucket, key)]
	if !ok {
		return nil, ObjectInfo{}, fmt.Errorf("storage: open object: %w", ErrNotFound)
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(o.data))), o.info(), nil
}

// Copy implements Objects.
func (m *Mem) Copy(_ context.Context, in CopyInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takeFailure(); err != nil {
		return err
	}
	o, ok := m.objects[memKey(in.SrcBucket, in.SrcKey)]
	if !ok {
		return fmt.Errorf("storage: copy object: %w", ErrNotFound)
	}
	m.objects[memKey(in.DstBucket, in.DstKey)] = memObject{
		data: bytes.Clone(o.data), contentType: in.ContentType, contentDisposition: in.ContentDisposition,
		cacheControl: in.CacheControl,
	}
	return nil
}

// Delete implements Objects.
func (m *Mem) Delete(_ context.Context, bucket, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.takeFailure(); err != nil {
		return err
	}
	delete(m.objects, memKey(bucket, key))
	return nil
}
