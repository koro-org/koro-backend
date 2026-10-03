package main

import (
	"context"
	"fmt"
)

// runWorker runs `koro worker`: asynq server, scheduler and outbox relay
// collected from the container (05 §8.5, §10.9). Filled by BE-006.
func runWorker(_ context.Context, _ []string) error {
	return fmt.Errorf("%w (BE-006)", errNotImplemented)
}
