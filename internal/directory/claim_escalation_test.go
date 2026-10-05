package directory_test

import (
	"errors"
	"slices"
	"testing"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/directory"
	"koro/internal/notify"
	"koro/internal/platform/problem"
)

// runTask runs the queued tasks of name through the module's handler.
func (w claimWorld) runTask(t *testing.T, name string) {
	t.Helper()
	var h func([]byte) error
	for _, task := range w.mod.Tasks() {
		if task.Name == name {
			h = func(p []byte) error { return task.Handle(t.Context(), p) }
		}
	}
	if h == nil {
		t.Fatalf("no task %s", name)
	}
	for _, q := range w.queue.Tasks() {
		if q.Type == name {
			if err := h(q.Payload); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
	}
}

// runJob runs the scheduled job name once.
func (w claimWorld) runJob(t *testing.T, name string) {
	t.Helper()
	for _, j := range w.mod.Jobs() {
		if j.Name == name {
			if err := j.Handle(t.Context()); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return
		}
	}
	t.Fatalf("no job %s", name)
}

// sent returns the recipients of template, in sending order.
func (w claimWorld) sent(template string) []uuid.UUID {
	w.sender.mu.Lock()
	defer w.sender.mu.Unlock()
	var out []uuid.UUID
	for _, m := range w.sender.msgs {
		if m.Template == template {
			out = append(out, *m.UserID)
		}
	}
	return out
}

// TestClaim_RepNotifiedThenEscalatedAfter7Days pins 06 H-58: the rep who
// decides a claim is told when it is made and reminded on the 5th day; from
// the 7th day on the claim is platform staff's (the platform admin decides,
// the rep neither sees nor decides it) and the platform admins are told
// once. Every notice is an in-app notice deduplicated per claim and
// recipient.
func TestClaim_RepNotifiedThenEscalatedAfter7Days(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	rep := w.rep(t, w.uni)
	c := w.claim(t, w.student(t, w.uni, false), w.club.Club.OrgID)

	w.runTask(t, directory.ClaimCreatedTaskName)
	if got := w.sent("directory.claim_pending"); !slices.Equal(got, []uuid.UUID{rep.UserID}) {
		t.Fatalf("claim_pending to %v, want the rep", got)
	}
	for _, m := range w.sender.msgs {
		if m.Channel != notify.ChannelInApp || m.Category != notify.CategoryTransactional ||
			m.DedupeKey != "directory.claim_pending:"+c.ID.String()+":"+rep.UserID.String() || m.Data["claim_id"] != c.ID {
			t.Fatalf("notice = %+v", m)
		}
	}

	// Day 4: nothing yet. Day 5: the reminder, once.
	w.clk.Set(c.CreatedAt.Add(4 * 24 * time.Hour))
	w.runJob(t, directory.ClaimRemindersJobName)
	if got := w.sent("directory.claim_reminder"); len(got) != 0 {
		t.Fatalf("reminded on day 4: %v", got)
	}
	w.clk.Set(c.CreatedAt.Add(directory.ClaimReminderAfter + time.Hour))
	w.runJob(t, directory.ClaimRemindersJobName)
	if got := w.sent("directory.claim_reminder"); !slices.Equal(got, []uuid.UUID{rep.UserID}) {
		t.Fatalf("claim_reminder to %v, want the rep", got)
	}
	if v, err := w.svc.AdminGetClubClaim(t.Context(), rep, c.ID); err != nil || v.Approver != authz.ClaimApproverRep {
		t.Fatalf("day 5: rep view = %+v, %v", v.Approver, err)
	}

	// Day 7: platform staff's. The rep's queue and reads drop it, the
	// platform admin decides it and the admins are told.
	w.clk.Set(c.CreatedAt.Add(directory.ClaimEscalateAfter))
	w.runJob(t, directory.ClaimRemindersJobName)
	if got := w.sent("directory.claim_escalated"); !slices.Equal(got, w.admins) {
		t.Fatalf("claim_escalated to %v, want the platform admins %v", got, w.admins)
	}
	if got := w.sent("directory.claim_reminder"); len(got) != 1 {
		t.Fatalf("reminded after the escalation: %v", got)
	}
	p, err := w.svc.AdminListClubClaims(t.Context(), rep, directory.AdminClaimQuery{})
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("rep queue after the escalation = %d items, %v", len(p.Items), err)
	}
	if _, err := w.svc.AdminGetClubClaim(t.Context(), rep, c.ID); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("rep reads an escalated claim: %v", err)
	}
	if _, err := w.svc.AdminDecideClubClaim(t.Context(), rep, c.ID, approveIn()); !errors.Is(err, problem.ErrNotFound) {
		t.Fatalf("rep decides an escalated claim: %v", err)
	}
	v, err := w.svc.AdminDecideClubClaim(t.Context(), pad(t), c.ID, approveIn())
	if err != nil || v.Claim.Status != directory.ClaimApproved || v.Approver != authz.ClaimApproverPlatformAdmin {
		t.Fatalf("platform admin decision = %+v, %v", v, err)
	}
	// A decided claim is in nobody's reminders.
	w.clk.Set(c.CreatedAt.Add(8 * 24 * time.Hour))
	w.runJob(t, directory.ClaimRemindersJobName)
	if got := w.sent("directory.claim_escalated"); len(got) != len(w.admins) {
		t.Fatalf("notified again after the decision: %v", got)
	}
}

// TestClaim_NoRepNoReminder: a claim of a university without a rep, or of a
// sensitive club, is platform staff's from the start: the creation notice
// and the daily job tell nobody.
func TestClaim_NoRepNoReminder(t *testing.T) {
	t.Parallel()
	w := newClaimWorld(t)
	w.rep(t, w.uni)
	sens := w.claim(t, w.student(t, w.uni, false), w.sens.Club.OrgID)
	otherClub := w.env.club(t, w.other, "Öteki Kulüp", directory.CategoryHobby)
	w.claim(t, w.student(t, w.other, false), otherClub.Club.OrgID)
	w.runTask(t, directory.ClaimCreatedTaskName)
	for _, d := range []time.Duration{directory.ClaimReminderAfter, directory.ClaimEscalateAfter} {
		w.clk.Set(sens.CreatedAt.Add(d + time.Hour))
		w.runJob(t, directory.ClaimRemindersJobName)
	}
	if len(w.sender.msgs) != 0 {
		t.Fatalf("notices = %+v", w.sender.msgs)
	}
	var crons []string
	for _, j := range w.mod.Jobs() {
		crons = append(crons, j.Name+" "+j.Cron)
	}
	if !slices.Equal(crons, []string{"directory.claim_reminders 0 9 * * *"}) {
		t.Fatalf("jobs = %v", crons)
	}
}
