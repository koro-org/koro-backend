// Package filestest helps the owning modules test their file.rejected
// subscriptions (T-176, BE-151, BE-154).
package filestest

import (
	"testing"
	"uuid"

	"koro/internal/files"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/outbox"
)

// Reject delivers file.rejected of file (its purpose and reason) attached
// to module's entity to the subscription name that subscribe registers, as
// the worker would, and returns the handler's error. A missing subscription
// fails t.
func Reject(t testing.TB, subscribe func(*outbox.Registry), name string, file uuid.UUID, purpose files.Purpose,
	reason files.RejectReason, module string, entity uuid.UUID) error {
	t.Helper()
	r := outbox.NewRegistry()
	subscribe(r)
	env, err := domainevent.New(domainevent.FileRejected{FileID: file, Purpose: string(purpose), Reason: string(reason),
		AttachedModule: module, AttachedEntityID: &entity}, "file", file)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range r.For(domainevent.TopicFileRejected) {
		if s.Name == name {
			return s.Handle(t.Context(), env)
		}
	}
	t.Fatalf("file.rejected has no %s subscription", name)
	return nil
}
