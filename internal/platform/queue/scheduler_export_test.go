//go:build integration

package queue

import "github.com/hibiken/asynq"

// RegisterForTest records the real Register arguments while still registering
// with asynq. Only the integration test binary exposes this observation seam.
func RegisterForTest(s *Scheduler, j Job) (*asynq.Task, []asynq.Option, error) {
	original := s.s
	recorder := &recordingScheduler{schedulerBackend: original}
	s.s = recorder
	defer func() { s.s = original }()
	err := s.Register(j)
	return recorder.task, recorder.opts, err
}

type recordingScheduler struct {
	schedulerBackend
	task *asynq.Task
	opts []asynq.Option
}

func (s *recordingScheduler) Register(spec string, task *asynq.Task, opts ...asynq.Option) (string, error) {
	s.task, s.opts = task, opts
	return s.schedulerBackend.Register(spec, task, opts...)
}
