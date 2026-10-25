package consent

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
)

// İYS alarms (BE-105): the cases the DPO acts on, logged at ERROR with
// alarm=true and an iys_alarm reason, and counted for
// koro_consent_iys_alarms_total{reason} (cmd/koro wires the metric).
const (
	// IYSAlarmTooLate: İYS refused a consent older than three business days
	// (H467 behind the port); the grant is not valid for commercial
	// messages (Yönetmelik m.7/12).
	IYSAlarmTooLate = "too_late"
	// IYSAlarmConflict: İYS kept an older refusal past IYSInFlightGrace; the
	// grant was withdrawn.
	IYSAlarmConflict = "conflict"
	// IYSAlarmSweepUnavailable: the change feed lost changes and the
	// adapter cannot read single states to reconcile.
	IYSAlarmSweepUnavailable = "sweep_unavailable"
	// IYSAlarmReadRejected: İYS refused a read for good (credentials,
	// subscription, a malformed request).
	IYSAlarmReadRejected = "read_rejected"
)

// iysAlarms counts the İYS alarms of the process.
type iysAlarms struct {
	tooLate, conflict, sweepUnavailable, readRejected atomic.Int64
}

// IYSAlarm is one counter of Module.IYSAlarms.
type IYSAlarm struct {
	Reason string
	Count  int64
}

// IYSAlarms returns the İYS alarm counters of the process (monotonic), in a
// fixed reason order.
func (m Module) IYSAlarms() []IYSAlarm {
	if m.svc == nil {
		return nil
	}
	a := &m.svc.alarms
	return []IYSAlarm{
		{IYSAlarmTooLate, a.tooLate.Load()}, {IYSAlarmConflict, a.conflict.Load()},
		{IYSAlarmSweepUnavailable, a.sweepUnavailable.Load()}, {IYSAlarmReadRejected, a.readRejected.Load()},
	}
}

// watchedIYS raises the too-late alarm of any İYS adapter's Submit.
type watchedIYS struct {
	IYSClient
	s *service
}

// Submit implements IYSClient.
func (w watchedIYS) Submit(ctx context.Context, r IYSRecord) (IYSReceipt, error) {
	rc, err := w.IYSClient.Submit(ctx, r)
	if errors.Is(err, ErrIYSTooLate) {
		w.s.alarms.tooLate.Add(1)
		w.s.d.Log.LogAttrs(ctx, slog.LevelError,
			"iys refused a consent older than three business days: it is not valid for commercial messages",
			slog.Bool("alarm", true), slog.String("iys_alarm", IYSAlarmTooLate), slog.String("submission_id", r.SubmissionID.String()),
			slog.String("consent_id", r.ConsentID.String()), slog.String("channel", string(r.Channel)),
			slog.String("action", string(r.Action)))
	}
	return rc, err
}
