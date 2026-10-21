package talent_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"uuid"

	"koro/internal/authz"
	"koro/internal/platform/domainevent"
	"koro/internal/students"
	"koro/internal/talent"
)

// TestNeutralize_Objection_CannotBeLiftedByAdmin (KVKK md. 11/1-g, 02 §15.8,
// 04 §T liftTalentNeutralization 409): an objection neutralises the
// subject for good. No staff persona lifts it (Y1 refuses every role but
// talent_auditor, and the auditor's lift refuses an objection by rule even
// with the right proof: 409 talent.objection_permanent), the student's own
// ranking switch does not lift it, and the subject stays neutral and
// collects nothing.
func TestNeutralize_Objection_CannotBeLiftedByAdmin(t *testing.T) {
	t.Parallel()
	e := buildEnvWith(t, envOptions{collection: true, auditorAPI: true, config: true})
	user := e.student()
	e.store.PutScore(user, 2, false)
	dsr := uuid.NewV7()
	ev := envelope(t, domainevent.DSRProfilingObjection{DSRID: dsr, UserID: user}, t0)
	e.deliver(t, ev)
	e.deliver(t, ev) // idempotent: one active row
	act := e.active(user)
	if len(act) != 1 || act[0].Reason != talent.NeutralizeObjection || act[0].DSRID == nil || *act[0].DSRID != dsr {
		t.Fatalf("active neutralisations = %+v", act)
	}
	obj := act[0]

	for _, p := range []string{"platform_admin", "talent_auditor", "dpo", "support", "student"} {
		_, err := e.mod.Admin.LiftNeutralization(t.Context(), persona(t, p), obj.ID,
			talent.NeutralizationInput{Reason: "KVKK itirazı incelemesi"}, goodProof)
		want := authz.ErrForbidden
		if p == "talent_auditor" {
			want = talent.ErrObjectionPermanent
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s lifting an objection: %v, want %v", p, err, want)
		}
	}
	if err := obj.Liftable(); !errors.Is(err, talent.ErrObjectionPermanent) {
		t.Fatalf("Liftable(objection) = %v, want ErrObjectionPermanent", err)
	}
	// The student turns ranking off and on again: only the opt-out comes
	// and goes, the objection stays.
	e.students.setChoice(user, students.RankingDisabled)
	e.send(t, domainevent.StudentRankingChoiceChanged{UserID: user, Choice: "disabled"}, t0)
	e.students.setChoice(user, students.RankingEnabled)
	e.send(t, domainevent.StudentRankingChoiceChanged{UserID: user, Choice: "enabled"}, t0)
	if act := e.active(user); len(act) != 1 || act[0].ID != obj.ID {
		t.Fatalf("after the student's switch: active = %+v", act)
	}
	bands, err := e.mod.Ranking.Bands(t.Context(), []uuid.UUID{user})
	if err != nil || len(bands) != 0 {
		t.Fatalf("Bands of an objector = %d entries, %v; want neutral", len(bands), err)
	}
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
	if rows := e.points(user); len(rows) != 0 || e.count(talent.SignalFeedLike, talent.OutcomeNeutralized) != 1 {
		t.Fatalf("an objector's signal was written: %+v", rows)
	}
	// Each change was audited as talent.neutralization.manage by its
	// system job (no duplicate record for the redelivery).
	if got := e.audit.actions(); len(got) != 3 || !allEqual(got, string(authz.TalentNeutralizationManage)) {
		t.Fatalf("audit actions = %q, want 3 × talent.neutralization.manage (objection, opt-out, lift)", got)
	}
}

// TestNeutralize_RankingChoice_OptOutFollowsTheStudent: disabled → an
// opt-out neutralisation (no signals while it is on); enabled → lifted.
// The consumer reads the current choice, so events out of order converge.
func TestNeutralize_RankingChoice_OptOutFollowsTheStudent(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user := e.student()
	e.students.setChoice(user, students.RankingDisabled)
	e.send(t, domainevent.StudentRankingChoiceChanged{UserID: user, Choice: "disabled"}, t0)
	act := e.active(user)
	if len(act) != 1 || act[0].Reason != talent.NeutralizeOptOut {
		t.Fatalf("opt-out: active = %+v", act)
	}
	if err := act[0].Liftable(); !errors.Is(err, talent.ErrOptOutStudentOwned) {
		t.Fatalf("Liftable(opt_out) = %v", err)
	}
	// A stale "disabled" delivered after the student turned it on again
	// does not neutralise.
	e.students.setChoice(user, students.RankingDefault)
	e.send(t, domainevent.StudentRankingChoiceChanged{UserID: user, Choice: "default"}, t0)
	e.send(t, domainevent.StudentRankingChoiceChanged{UserID: user, Choice: "disabled"}, t0)
	if act := e.active(user); len(act) != 0 {
		t.Fatalf("after enabling: active = %+v", act)
	}
	all := e.store.AllNeutralizations()
	if len(all) != 1 || all[0].LiftedAt == nil || all[0].LiftedBy != nil {
		t.Fatalf("history = %+v, want one lifted opt-out (system, no lifted_by)", all)
	}
}

// TestNeutralize_HookRules: the kvkk hook validates the reason; an admin
// neutralisation is the liftable one, by the talent_auditor alone.
func TestNeutralize_HookRules(t *testing.T) {
	t.Parallel()
	e := buildEnvWith(t, envOptions{auditorAPI: true, config: true})
	user := e.student()
	ctx := t.Context()
	for _, bad := range []struct {
		reason talent.NeutralizeReason
		dsr    *uuid.UUID
	}{{"minor", nil}, {"", nil}, {talent.NeutralizeObjection, nil}} {
		if err := e.mod.Auditor.Neutralize(ctx, user, bad.reason, bad.dsr); !errors.Is(err, talent.ErrInvalidReason) {
			t.Fatalf("Neutralize(%q) = %v, want ErrInvalidReason", bad.reason, err)
		}
	}
	if err := e.mod.Auditor.Neutralize(ctx, user, talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	act := e.active(user)
	if len(act) != 1 || act[0].Liftable() != nil {
		t.Fatalf("admin neutralisation = %+v, liftable %v", act, act[0].Liftable())
	}
	in := talent.NeutralizationInput{Reason: "Kötüye kullanım incelemesi bitti"}
	if _, err := e.mod.Admin.LiftNeutralization(ctx, persona(t, "platform_admin"), act[0].ID, in, goodProof); !errors.Is(err, authz.ErrForbidden) {
		t.Fatalf("platform_admin lift = %v, want ErrForbidden (Y1)", err)
	}
	lifted, err := e.mod.Admin.LiftNeutralization(ctx, persona(t, "talent_auditor"), act[0].ID, in, goodProof)
	if err != nil || lifted.Active() || lifted.LiftedBy == nil || *lifted.LiftedBy != persona(t, "talent_auditor").UserID {
		t.Fatalf("auditor lift = %+v, %v", lifted, err)
	}
	if _, err := e.mod.Admin.LiftNeutralization(ctx, persona(t, "talent_auditor"), act[0].ID, in, goodProof); !errors.Is(err, talent.ErrNotActive) {
		t.Fatalf("second lift = %v, want ErrNotActive", err)
	}
	if err := (talent.Neutralization{LiftedAt: ptr(t0)}).Liftable(); !errors.Is(err, talent.ErrNotActive) {
		t.Fatalf("Liftable(lifted) = %v", err)
	}
}

// TestReport_SubjectReportAudited (03 §5.12, 6.5): the profiling_access
// report lists the signals by category with the band and the
// neutralisations, renders the band explicitly, and every read is audited
// as talent.report.read; the auditor endpoint is closed (404) for every
// persona while talent.auditor_api_enabled is false.
func TestReport_SubjectReportAudited(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user := e.student()
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
	e.send(t, domainevent.MembershipStarted{MembershipID: uuid.NewV7(), OrgID: uuid.NewV7(), OrgKind: "club", UserID: user, Role: "member"}, t0)
	e.store.PutScore(user, 2, false)
	approver := uuid.NewV7()
	raw, err := e.mod.Auditor.SubjectReport(t.Context(), approver, user, "dsr:"+uuid.NewV7().String())
	if err != nil {
		t.Fatal(err)
	}
	var rep struct {
		UserID     string `json:"user_id"`
		Band       int    `json:"band"`
		Scored     bool   `json:"scored"`
		Purpose    string `json:"purpose"`
		Categories []struct {
			Category string `json:"category"`
			Signals  int    `json:"signals"`
			PointsCP int    `json:"points_cp"`
		} `json:"categories"`
		Signals []struct {
			SignalCode string `json:"signal_code"`
			Category   string `json:"category"`
		} `json:"signals"`
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.UserID != user.String() || rep.Band != 2 || !rep.Scored || rep.Purpose == "" || len(rep.Signals) != 2 || len(rep.Categories) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if rep.Categories[0].Category != talent.CategoryCommunity || rep.Categories[0].PointsCP != 200 || rep.Categories[1].PointsCP != 10 {
		t.Fatalf("categories = %+v", rep.Categories)
	}
	e.audit.mu.Lock()
	last := e.audit.events[len(e.audit.events)-1]
	e.audit.mu.Unlock()
	if last.Action != string(authz.TalentReportRead) || last.ActorUserID == nil || *last.ActorUserID != approver ||
		last.ResourceID == nil || *last.ResourceID != user || last.Reason == "" {
		t.Fatalf("audit = %+v", last)
	}
	// A neutralised subject's report shows band 0.
	if err := e.mod.Auditor.Neutralize(t.Context(), user, talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	raw, err = e.mod.Auditor.SubjectReport(t.Context(), uuid.Nil(), user, "dsr:x")
	if err != nil || json.Unmarshal(raw, &rep) != nil || rep.Band != 0 {
		t.Fatalf("neutralised report band = %d, %v", rep.Band, err)
	}
	for _, p := range []string{"platform_admin", "talent_auditor", "dpo", "student"} {
		if _, err := e.mod.Admin.Report(t.Context(), persona(t, p), user, "reason long enough"); !errors.Is(err, talent.ErrNotFound) {
			t.Fatalf("Report as %s with the API closed = %v, want ErrNotFound", p, err)
		}
	}
}

// TestBands_StoredBandUnlessNeutralised: Bands returns the stored band
// above neutral; a missing score, a neutralised score or an active
// neutralisation is neutral (missing from the map).
func TestBands_StoredBandUnlessNeutralised(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	a, b, c, d := e.student(), e.student(), e.student(), e.student()
	e.store.PutScore(a, 3, false)
	e.store.PutScore(b, 2, true)
	e.store.PutScore(c, 1, false)
	if err := e.mod.Auditor.Neutralize(t.Context(), c, talent.NeutralizeAdmin, nil); err != nil {
		t.Fatal(err)
	}
	bands, err := e.mod.Ranking.Bands(t.Context(), []uuid.UUID{a, b, c, d})
	if err != nil {
		t.Fatal(err)
	}
	if len(bands) != 1 || bands[a] != talent.Band(3) || bands[d] != talent.BandNeutral {
		t.Fatalf("bands has %d entries", len(bands))
	}
}

// TestPrivacy_ExportStatesOnlyAndEraseDeletesAll (01 §3.3, 03 §5.12): the
// access export says whether profiling data is held, nothing more; the
// erasure deletes every talent row, the ledger only for a completed
// erasure request, and is idempotent.
func TestPrivacy_ExportStatesOnlyAndEraseDeletesAll(t *testing.T) {
	t.Parallel()
	e := newEnv(t, true)
	user, other := e.student(), e.student()
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: user, AuthorOrgID: uuid.NewV7()}, t0)
	e.send(t, domainevent.PostLiked{PostID: uuid.NewV7(), UserID: other, AuthorOrgID: uuid.NewV7()}, t0)
	if err := e.mod.Auditor.Neutralize(t.Context(), user, talent.NeutralizeObjection, ptr(uuid.NewV7())); err != nil {
		t.Fatal(err)
	}
	e.store.PutScore(user, 1, false)
	sec, err := e.mod.Privacy.Export(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if e.mod.Privacy.Name() != "talent" || sec.Module != "talent" || len(sec.Items) != 1 || sec.Items[0]["profiling_data_held"] != true {
		t.Fatalf("export = %+v", sec)
	}
	for k := range sec.Items[0] {
		if !slices.Contains([]string{"profiling_data_held", "disclosure", "note"}, k) {
			t.Fatalf("export carries %q", k)
		}
	}
	e.store.ErasedUsers = map[uuid.UUID]bool{}
	if _, err := e.mod.Privacy.Erase(t.Context(), user); err == nil {
		t.Fatal("ledger erased without a completed erasure request")
	}
	if len(e.points(user)) != 1 || len(e.active(user)) != 1 {
		t.Fatal("a refused erasure deleted rows")
	}
	e.store.ErasedUsers[user] = true
	res, err := e.mod.Privacy.Erase(t.Context(), user)
	if err != nil || res.Module != "talent" || res.Deleted < 4 {
		t.Fatalf("Erase = %+v, %v", res, err)
	}
	if len(e.points(user)) != 0 || len(e.store.AllNeutralizations()) != 0 {
		t.Fatal("rows left after the erasure")
	}
	if len(e.points(other)) != 1 {
		t.Fatal("another user's ledger was erased")
	}
	if res, err := e.mod.Privacy.Erase(t.Context(), user); err != nil || res.Deleted != 0 {
		t.Fatalf("second Erase = %+v, %v", res, err)
	}
	sec, err = e.mod.Privacy.Export(t.Context(), user)
	if err != nil || sec.Items[0]["profiling_data_held"] != false {
		t.Fatalf("export after erasure = %+v, %v", sec, err)
	}
}

func allEqual(xs []string, v string) bool {
	for _, x := range xs {
		if x != v {
			return false
		}
	}
	return true
}

func ptr[T any](v T) *T { return &v }
