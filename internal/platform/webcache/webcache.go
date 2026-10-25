// Package webcache is the publication webhook of Koro (BE-106): koro-web
// caches its public pages at the data level ("use cache" + cache tags), and
// when public content changes the worker tells it which tags to drop.
//
// Flow (01 §6.1 mechanism O): the producing module writes event.published,
// event.updated, event.cancelled, org.updated, club.updated,
// club.sensitivity_changed, club_claim.decided, file.published or
// file.rejected (an org logo / event cover, BE-120) into the outbox in its
// business transaction → the relay enqueues one asynq task per
// subscription (webcache.revalidate_*) → the handler resolves the current
// slugs (Resolver), builds the tags and POSTs them, signed, to
// {KORO_WEB_INTERNAL_URL}/api/revalidate.
//
// Contract (koro-web src/lib/revalidate.ts, README "Yayın webhook'u"; the
// two sides must change together):
//
//	POST /api/revalidate
//	X-Koro-Timestamp: <unix seconds>              (koro-web accepts ±300 s)
//	X-Koro-Signature: sha256=<hex(HMAC-SHA256(KORO_REVALIDATE_SECRET, "<timestamp>.<raw body>"))>
//	{"tags":["club:<slug>","company:<slug>","event:<slug>","sitemap"]}
//
// At most 64 tags per request, each matching ^[a-z0-9][a-z0-9:_./-]{0,127}$,
// body at most 16 KiB. The body carries public slugs and fixed tag names
// only: never personal data.
//
// Failures: network errors, 401 (secret rotation in progress), 404 (web not
// deployed yet), 408, 425, 429 and 5xx (503: the web has no secret yet) are
// retried by asynq with its capped exponential back-off (queue.DefaultMaxRetry);
// any other answer is permanent (queue.Permanent). Either way an exhausted
// task is archived as a dead letter with the queue's ERROR log line and
// koro_queue_tasks_processed_total{result="failed"}: the task never loops.
package webcache

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// Wire contract (koro-web src/lib/revalidate.ts).
const (
	Path            = "/api/revalidate"
	TimestampHeader = "X-Koro-Timestamp"
	SignatureHeader = "X-Koro-Signature"
	MaxTags         = 64        // revalidateBody: tags.max(64)
	MaxBodyBytes    = 16 * 1024 // MAX_BODY_BYTES
)

// Tag names of koro-web (cacheTag calls of the public pages and sitemap.ts).
const (
	TagSitemap    = "sitemap"
	clubPrefix    = "club:"
	companyPrefix = "company:"
	eventPrefix   = "event:"
)

// Org kinds with a public page on the web (orgs.Kind values).
const (
	KindClub    = "club"
	KindCompany = "company"
)

var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9:_./-]{0,127}$`)

// ValidTag reports whether koro-web accepts tag.
func ValidTag(tag string) bool { return tagRe.MatchString(tag) }

// EventTag is the cache tag of an event's public page.
func EventTag(slug string) string { return eventPrefix + slug }

// OrgTag is the cache tag of an org's public page: club:<slug> or
// company:<slug>; "" for a kind without a public page (university,
// platform) or without a slug.
func OrgTag(kind, slug string) string {
	if slug == "" {
		return ""
	}
	switch kind {
	case KindClub:
		return clubPrefix + slug
	case KindCompany:
		return companyPrefix + slug
	}
	return ""
}

// Sign returns the X-Koro-Signature value of body sent at timestamp.
func Sign(secret []byte, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(timestamp))
	m.Write([]byte{'.'})
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}
