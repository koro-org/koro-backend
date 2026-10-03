// Package domainevent holds the domain event contracts of Koro (01 §3.2,
// §6.2): the Envelope, one topic constant and one payload struct per topic,
// and the catalog that says who produces each topic and how it is
// delivered. Producers and consumers import only this package, never each
// other (01 §5.4).
//
// Layout (BE-001 R6): every producing module owns one file
// (domainevent/<module>.go) with its topic constants, payload structs and a
// register(...) call. Module tickets edit only their own file; a new topic
// is added to 01 §6.2 first.
//
// Payload rules (01 §0-6, §5.4; 05 §8.4-1): identifiers (UUID), status
// codes, flags, counts and instants only. No name, e-mail, phone, form
// answer, token, talent value, band or score, and no free text. Fields are
// added, never renamed or removed (old tasks may still be queued); a
// breaking change bumps Spec.Version. The rules are enforced by
// TestDomainevent_PayloadsHaveNoPIIFields.
//
// The package is a leaf: it imports only the standard library and
// platform/civil.
package domainevent

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"
	"uuid"
)

// Envelope is one domain event. ID is the outbox row ID and the consumer's
// idempotency key (01 §6.1).
type Envelope struct {
	ID            uuid.UUID       `json:"id"`
	Topic         string          `json:"topic"`
	Version       int             `json:"version"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   uuid.UUID       `json:"aggregate_id"`
	OccurredAt    time.Time       `json:"occurred_at"`
	ActorUserID   *uuid.UUID      `json:"actor_user_id,omitempty"`
	RequestID     string          `json:"request_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
}

// Payload is implemented by every payload struct of the catalog.
type Payload interface {
	Topic() string
}

// Mechanism is the delivery mechanism of a topic (01 §6.1).
type Mechanism string

// Mechanisms.
const (
	// Outbox (O): written into the outbox table in the business transaction;
	// at least once, consumers are idempotent.
	Outbox Mechanism = "O"
	// AfterCommit (A): enqueued after the transaction commits; at most
	// once, nightly reconciliation jobs repair losses.
	AfterCommit Mechanism = "A"
)

// Spec describes one topic of the catalog.
type Spec struct {
	Topic     string
	Producer  string    // producing module (01 §2)
	Mechanism Mechanism // fixed per topic
	Version   int       // payload schema version (Envelope.Version)
	Payload   Payload   // zero value of the payload struct
}

// Errors.
var (
	ErrUnknownTopic  = errors.New("domainevent: topic is not in the catalog (01 §6.2)")
	ErrTopicMismatch = errors.New("domainevent: envelope topic does not match the payload type")
)

var (
	topicRe = regexp.MustCompile(`^[a-z_]+\.[a-z_]+$`) // outbox_topic_chk (02 §1.1)

	catalogMu sync.RWMutex
	catalog   = map[string]Spec{}
)

// spec declares a version-1 topic.
func spec(producer string, m Mechanism, p Payload) Spec {
	return Spec{Topic: p.Topic(), Producer: producer, Mechanism: m, Version: 1, Payload: p}
}

// register adds specs to the catalog at package initialisation. A malformed
// or duplicate topic panics: the catalog is static.
func register(specs ...Spec) bool {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	for _, s := range specs {
		switch {
		case !topicRe.MatchString(s.Topic):
			panic(fmt.Sprintf("domainevent: malformed topic %q", s.Topic))
		case s.Payload == nil || s.Payload.Topic() != s.Topic:
			panic(fmt.Sprintf("domainevent: payload of %q reports another topic", s.Topic))
		case s.Mechanism != Outbox && s.Mechanism != AfterCommit:
			panic(fmt.Sprintf("domainevent: topic %q has no mechanism", s.Topic))
		case s.Version < 1:
			panic(fmt.Sprintf("domainevent: topic %q has version %d", s.Topic, s.Version))
		case s.Producer == "":
			panic(fmt.Sprintf("domainevent: topic %q has no producer", s.Topic))
		}
		if _, dup := catalog[s.Topic]; dup {
			panic(fmt.Sprintf("domainevent: duplicate topic %q", s.Topic))
		}
		catalog[s.Topic] = s
	}
	return true
}

// Lookup returns the spec of topic.
func Lookup(topic string) (Spec, bool) {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	s, ok := catalog[topic]
	return s, ok
}

// Catalog returns every spec sorted by topic.
func Catalog() []Spec {
	catalogMu.RLock()
	defer catalogMu.RUnlock()
	out := make([]Spec, 0, len(catalog))
	for _, t := range slices.Sorted(maps.Keys(catalog)) {
		out = append(out, catalog[t])
	}
	return out
}

// Encode marshals a payload.
func Encode(p Payload) (json.RawMessage, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("domainevent: encode %s: %w", p.Topic(), err)
	}
	return b, nil
}

// New builds the envelope of p for outbox.Writer: topic, version and payload
// are filled; ID, OccurredAt, ActorUserID and RequestID are filled by the
// writer from the clock and the request context.
func New(p Payload, aggregateType string, aggregateID uuid.UUID) (Envelope, error) {
	s, ok := Lookup(p.Topic())
	if !ok {
		return Envelope{}, fmt.Errorf("%w: %q", ErrUnknownTopic, p.Topic())
	}
	b, err := Encode(p)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		Topic:         s.Topic,
		Version:       s.Version,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		Payload:       b,
	}, nil
}

// Decode unmarshals the payload of e into P. Unknown fields are ignored
// (a newer producer may have added fields).
func Decode[P Payload](e Envelope) (P, error) {
	var p P
	if e.Topic != p.Topic() {
		return p, fmt.Errorf("%w: %q into %T", ErrTopicMismatch, e.Topic, p)
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return p, fmt.Errorf("domainevent: decode %s: %w", e.Topic, err)
	}
	return p, nil
}
