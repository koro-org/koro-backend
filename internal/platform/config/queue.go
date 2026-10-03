package config

// Queue configures Redis and the asynq worker/scheduler (internal/platform/queue; 05 §8.5: REDIS_URL, WORKER_SCHEDULER_ENABLED).
// Empty until BE-006 adds its fields (BE-001 R7: only the owning ticket edits this file).
type Queue struct{}
