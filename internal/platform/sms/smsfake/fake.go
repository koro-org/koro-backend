// Package smsfake is the in-memory SMS adapter (SMS_PROVIDER=mem; BE-172):
// the adapter of tests and of local development. Nothing leaves the
// process: an accepted message is kept in memory (Sent, Last). It follows
// the rules of internal/platform/sms (smscontract pins them): a message
// sms.CheckMessage refuses is never kept, and a failure set with FailNext
// is returned once instead of the next send. It never logs.
package smsfake

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"

	"koro/internal/platform/sms"
)

// Name is the adapter's name (SMS_PROVIDER).
const Name = "mem"

// Fake is the in-memory adapter; safe for concurrent use.
type Fake struct {
	mu   sync.Mutex
	sent []sms.Message
	next []error
	seq  int
}

// New returns an empty Fake.
func New() *Fake { return &Fake{} }

// Name names the adapter.
func (*Fake) Name() string { return Name }

// Send implements the consumers' sender port.
func (f *Fake) Send(ctx context.Context, m sms.Message) (sms.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return sms.Receipt{}, err
	}
	if err := sms.CheckMessage(m); err != nil {
		return sms.Receipt{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.next) > 0 {
		err := f.next[0]
		f.next = f.next[1:]
		return sms.Receipt{}, err
	}
	f.seq++
	f.sent = append(f.sent, m)
	return sms.Receipt{ID: "mem-" + strconv.Itoa(f.seq)}, nil
}

// FailNext makes the next send return err (queued: one per call).
func (f *Fake) FailNext(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next = append(f.next, err)
}

// Sent returns the accepted messages, oldest first.
func (f *Fake) Sent() []sms.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// Last returns the last accepted message to number (E.164).
func (f *Fake) Last(number string) (sms.Message, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.sent) - 1; i >= 0; i-- {
		if f.sent[i].To == number {
			return f.sent[i], true
		}
	}
	return sms.Message{}, false
}

// String keeps the messages out of %v.
func (f *Fake) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fmt.Sprintf("smsfake.Fake{%d sent}", len(f.sent))
}
