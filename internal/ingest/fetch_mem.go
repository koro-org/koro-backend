package ingest

import (
	"context"
	"fmt"
	"slices"
	"sync"
)

// MemFetcher is the in-memory Fetcher of the service, wiring and matrix
// tests: no network. Pages are set by URL; an unknown URL is a 404;
// Disallowed URLs are refused by "robots.txt". It records the fetched URLs.
type MemFetcher struct {
	mu         sync.Mutex
	pages      map[string]Page
	disallowed map[string]bool
	errs       map[string]error
	fetched    []string
}

var _ Fetcher = (*MemFetcher)(nil)

// NewMemFetcher returns an empty fetcher.
func NewMemFetcher() *MemFetcher {
	return &MemFetcher{pages: map[string]Page{}, disallowed: map[string]bool{}, errs: map[string]error{}}
}

// Set serves p at url (p.URL defaults to url, p.Status to 200).
func (f *MemFetcher) Set(url string, p Page) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.URL == "" {
		p.URL = url
	}
	if p.Status == 0 {
		p.Status = 200
	}
	f.pages[url] = p
}

// Disallow makes robots.txt refuse url.
func (f *MemFetcher) Disallow(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disallowed[url] = true
}

// Fail makes fetching url fail with err.
func (f *MemFetcher) Fail(url string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[url] = err
}

// Fetched returns the URLs fetched so far, in order.
func (f *MemFetcher) Fetched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.fetched)
}

// Robots implements Fetcher.
func (f *MemFetcher) Robots(_ context.Context, url string) (RobotsVerdict, error) {
	if _, _, err := ParseSourceURL(url); err != nil {
		return RobotsVerdict{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return RobotsVerdict{Allowed: !f.disallowed[url]}, nil
}

// Fetch implements Fetcher (ETag: a matching If-None-Match is a 304).
func (f *MemFetcher) Fetch(_ context.Context, req FetchRequest) (Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, req.URL)
	if err := f.errs[req.URL]; err != nil {
		return Page{}, err
	}
	p, ok := f.pages[req.URL]
	if !ok {
		return Page{URL: req.URL, Status: 404}, nil
	}
	if p.ETag != "" && req.ETag == p.ETag {
		return Page{URL: p.URL, Status: 304, NotModified: true, ETag: p.ETag}, nil
	}
	if len(req.Accept) > 0 && p.Status >= 200 && p.Status < 300 && !slices.Contains(req.Accept, p.ContentType) {
		return p, fmt.Errorf("%w: %s", ErrContentType, p.ContentType)
	}
	return p, nil
}
