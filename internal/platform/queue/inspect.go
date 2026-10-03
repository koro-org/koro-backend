package queue

import (
	"fmt"
	"slices"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// Length is the number of tasks of one queue in one state.
type Length struct {
	Queue string // critical | default | low
	State string // pending | active | scheduled | retry | archived
	N     int
}

// Inspector reads queue lengths for the koro_queue_length gauge (BE-058).
type Inspector struct{ i *asynq.Inspector }

// NewInspector returns an inspector on rdb (shared pool; never closed here).
func NewInspector(rdb redis.UniversalClient) *Inspector {
	return &Inspector{i: asynq.NewInspectorFromRedisClient(rdb)}
}

// Lengths returns every Koro queue × state, with zeros for a queue that
// has not seen a task yet.
func (in *Inspector) Lengths() ([]Length, error) {
	known, err := in.i.Queues()
	if err != nil {
		return nil, fmt.Errorf("queue: list queues: %w", err)
	}
	var out []Length
	for _, q := range []string{Critical, Default, Low} {
		var info asynq.QueueInfo
		if slices.Contains(known, q) {
			qi, err := in.i.GetQueueInfo(q)
			if err != nil {
				return nil, fmt.Errorf("queue: inspect %s: %w", q, err)
			}
			info = *qi
		}
		out = append(out,
			Length{q, "pending", info.Pending}, Length{q, "active", info.Active},
			Length{q, "scheduled", info.Scheduled}, Length{q, "retry", info.Retry},
			Length{q, "archived", info.Archived},
		)
	}
	return out, nil
}
