package talent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/platform/db"
	"koro/internal/platform/httpx"
	"koro/internal/platform/problem"
)

// Admin is the talent_auditor surface (04 §T; 03 §5.12; BE-052): the only
// place a talent value is shown to a person. Every method:
//
//  1. answers ErrNotFound while talent.auditor_api_enabled is false, for
//     every actor and before anything else (k-flag: "registered but
//     closed", 03 Y1);
//  2. authorises (authz.Can: talent_auditor alone, MFA by role; the admin
//     surface is visible to every user, so anybody else gets 403 before a
//     row is read; platform_admin included, 03 Y1);
//  3. requires the access reason (X-Koro-Access-Reason, 10–500
//     characters; 422 otherwise) and validates the input;
//  4. for an R operation checks the re-authentication proof (03 §6.2): a
//     refused proof is one denied record of the action
//     (audit.ReauthFailed), an overload none;
//  5. reads or writes in a talent transaction and writes the audit record
//     of the action with the reason in the same transaction (no talent
//     value leaves without its record, 03 §6.5).
//
// The responses carry the subject's user ID only, never a name or e-mail.
// No method may be called inside a transaction.
type Admin interface {
	// Report is getTalentReport (talent.report.read).
	Report(ctx context.Context, a authz.Actor, userID uuid.UUID, reason string) (Report, error)
	// Distribution is getTalentDistribution (talent.distribution.read):
	// the users by band (k ≥ 10), optionally of one university.
	Distribution(ctx context.Context, a authz.Actor, in DistributionQuery) (Distribution, error)
	// CreateNeutralization is createTalentNeutralization
	// (talent.neutralization.manage, R): an auditor neutralisation
	// (reason admin) of userID.
	CreateNeutralization(ctx context.Context, a authz.Actor, userID uuid.UUID, in NeutralizationInput,
		proof identity.ReauthProof) (Neutralization, error)
	// LiftNeutralization is liftTalentNeutralization
	// (talent.neutralization.manage, R): only an auditor neutralisation is
	// lifted (an objection is permanent, an opt-out is the student's: 409).
	LiftNeutralization(ctx context.Context, a authz.Actor, id uuid.UUID, in NeutralizationInput,
		proof identity.ReauthProof) (Neutralization, error)
	// ReversePoint is reverseTalentPointEvent (talent.reversal.create, R):
	// an append-only reversal row of a ledger row.
	ReversePoint(ctx context.Context, a authz.Actor, pointID uuid.UUID, in ReversalInput,
		proof identity.ReauthProof) (PointEvent, error)
	// Config is getTalentConfig (talent.config.read): the version in force
	// and the signal catalogue.
	Config(ctx context.Context, a authz.Actor, reason string) (ConfigView, error)
	// CreateConfigVersion is createTalentConfigVersion
	// (talent.config.manage, R): the signal updates and a new version in
	// force from now (the next nightly run scores with it; every ledger
	// row written from now on is frozen with it).
	CreateConfigVersion(ctx context.Context, a authz.Actor, in ConfigInput, proof identity.ReauthProof) (ConfigView, error)
	// Status is getTalentStatus (talent.config.read; BE-129): whether the
	// auditor API is open. The one exception to steps 1, 3 and 4 above: it
	// answers with the flag off too, needs no access reason and writes no
	// record (it reads no talent data).
	Status(ctx context.Context, a authz.Actor) (bool, error)
}

// DistributionQuery is the input of getTalentDistribution: the access
// reason and the optional university filter (a UUID text, parsed after
// the flag and authorisation).
type DistributionQuery struct {
	Reason       string
	UniversityID string
}

// Distribution is the result of getTalentDistribution: one bucket per
// band 0..3. Count is nil below the k ≥ 10 threshold (04 §1.11).
type Distribution struct {
	UniversityID *uuid.UUID
	Buckets      []BandBucket
}

// BandBucket is the number of users of one band.
type BandBucket struct {
	Band  int // explicit int: the auditor's view, never a Band (which refuses every encoder)
	Count *int
}

// kThreshold is the smallest group a count is shown for (04 §1.11).
const kThreshold = 10

// NeutralizationInput is the body of createTalentNeutralization and
// liftTalentNeutralization: the auditor's note (stored with a new
// neutralisation; a lift's note goes to the audit record's metadata,
// which audit never shows for talent.* records) and the access reason.
type NeutralizationInput struct {
	Reason string // X-Koro-Access-Reason
	Note   string
}

// ReversalInput is the body of reverseTalentPointEvent: why the row is
// wrong (to the audit record's metadata; the reversal row's reason_code is
// ReasonAuditorReversal) and the access reason.
type ReversalInput struct {
	Reason string // X-Koro-Access-Reason
	Why    string
}

// ConfigInput is the body of createTalentConfigVersion.
type ConfigInput struct {
	Reason  string // X-Koro-Access-Reason
	Params  ScoreParams
	Signals []SignalUpdate
	Notes   string
}

// ConfigView is getTalentConfig's result: the version in force (nil
// before the first one) and the signal catalogue.
type ConfigView struct {
	Active  *ConfigVersion
	Signals []SignalDef
}

// ReasonAuditorReversal is the reason_code of an auditor's reversal row.
const ReasonAuditorReversal = "auditor_reversal"

// Audit resource types of the records that name no subject.
const (
	resourceDistribution = "talent_distribution"
	resourceConfig       = "talent_config_version"
)

// maxNote bounds the auditor's texts (02 §15.8 note ≤ 500): the
// neutralisation note, the reversal reason and the config version notes
// (noteText in the contract). The access reason is httpx's (10–500).
const maxNote = 500

// adminSurface is the authz view of /admin/talent/** without a subject.
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface}

// subject is the authz view of a talent subject.
func subject(userID *uuid.UUID) authz.Resource {
	return authz.Resource{Type: authz.TypeTalentSubject, OwnerUserID: userID}
}

// gate is steps 1–3 of every Admin method: the flag, authz and the access
// reason (returned trimmed).
func (s *service) gate(ctx context.Context, a authz.Actor, act authz.Action, r authz.Resource, reason string) (string, error) {
	if !s.d.AuditorAPIEnabled {
		return "", ErrNotFound
	}
	if err := authz.Can(ctx, a, act, r); err != nil {
		return "", err
	}
	return httpx.RequireAccessReason(reason)
}

// text trims v and checks its length (min 0: optional).
func text(v, location string, minLen, maxLen int) (string, *problem.FieldError) {
	v = strings.TrimSpace(v)
	if n := utf8.RuneCountInString(v); n < minLen || n > maxLen {
		return "", &problem.FieldError{Location: location, Message: fmt.Sprintf("must be %d–%d characters", minLen, maxLen)}
	}
	return v, nil
}

// idOrNil is &id, or nil for the nil UUID (a malformed path ID).
func idOrNil(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil() {
		return nil
	}
	return &id
}

// invalid is the 422 of field errors.
func invalid(fe ...problem.FieldError) error { return problem.ErrValidation.WithFields(fe...) }

// Report implements Admin.
func (s *service) Report(ctx context.Context, a authz.Actor, userID uuid.UUID, reason string) (Report, error) {
	reason, err := s.gate(ctx, a, authz.TalentReportRead, subject(&userID), reason)
	if err != nil {
		return Report{}, err
	}
	return s.report(ctx, a, userID, reason)
}

// Distribution implements Admin. The counts exclude neutralised users (an
// objection stops the processing; 01 §1.4 analytics rule).
func (s *service) Distribution(ctx context.Context, a authz.Actor, in DistributionQuery) (Distribution, error) {
	reason, err := s.gate(ctx, a, authz.TalentDistributionRead, adminSurface, in.Reason)
	if err != nil {
		return Distribution{}, err
	}
	var out Distribution
	prefix := ""
	if raw := strings.TrimSpace(in.UniversityID); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil || id == uuid.Nil() {
			return Distribution{}, invalid(problem.FieldError{Location: "query.university_id", Message: "must be a UUID"})
		}
		out.UniversityID, prefix = &id, "uni:"+id.String()
	}
	err = s.inTx(ctx, func(ctx context.Context) error {
		counts, err := s.scores().BandCounts(ctx, prefix)
		if err != nil {
			return err
		}
		out.Buckets = make([]BandBucket, 0, int(BandMax)+1)
		for b := range int(BandMax) + 1 {
			bucket := BandBucket{Band: b}
			if n := counts[b]; n >= kThreshold {
				bucket.Count = &n
			}
			out.Buckets = append(out.Buckets, bucket)
		}
		meta := map[string]any{}
		if out.UniversityID != nil {
			meta["university_id"] = out.UniversityID.String()
		}
		return s.roles().AsApp(ctx, func(ctx context.Context) error {
			return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.TalentDistributionRead), Outcome: audit.Success,
				ResourceType: resourceDistribution, Reason: reason, Metadata: meta}.By(a))
		})
	})
	if err != nil {
		return Distribution{}, err
	}
	return out, nil
}

// CreateNeutralization implements Admin. The subject must exist (a user
// unknown to PostgreSQL is 404); one active auditor neutralisation per user
// (409 talent.already_neutralized).
func (s *service) CreateNeutralization(ctx context.Context, a authz.Actor, userID uuid.UUID, in NeutralizationInput,
	proof identity.ReauthProof) (Neutralization, error) {
	reason, err := s.gate(ctx, a, authz.TalentNeutralizationManage, subject(&userID), in.Reason)
	if err != nil {
		return Neutralization{}, err
	}
	note, fe := text(in.Note, "body.note", 1, maxNote)
	if fe != nil {
		return Neutralization{}, invalid(*fe)
	}
	if s.d.Reauth == nil {
		return Neutralization{}, ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		denied := audit.Event{Action: string(authz.TalentNeutralizationManage), ResourceType: authz.TypeTalentSubject,
			ResourceID: idOrNil(userID), Metadata: map[string]any{"change": changeNeutralized}}.By(a)
		return Neutralization{}, audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	if userID == uuid.Nil() {
		return Neutralization{}, ErrNotFound
	}
	by := a.UserID
	var out Neutralization
	err = s.inTx(ctx, func(ctx context.Context) error {
		row, inserted, err := s.neutralizations().InsertNeutralization(ctx, Neutralization{ID: uuid.NewV7(), UserID: userID,
			Reason: NeutralizeAdmin, Note: note, CreatedBy: &by})
		switch {
		case db.ConstraintName(err) == "neutralizations_user_id_fkey":
			return ErrNotFound.Wrap(err) // no such user
		case err != nil:
			return err
		case !inserted:
			return ErrAlreadyNeutralized
		}
		out = row
		return s.auditNeutralizationBy(ctx, a, row, changeNeutralized, reason, map[string]any{audit.MetaReauth: kind})
	})
	if err != nil {
		return Neutralization{}, err
	}
	return out, nil
}

// LiftNeutralization implements Admin.
func (s *service) LiftNeutralization(ctx context.Context, a authz.Actor, id uuid.UUID, in NeutralizationInput,
	proof identity.ReauthProof) (Neutralization, error) {
	reason, err := s.gate(ctx, a, authz.TalentNeutralizationManage, subject(nil), in.Reason)
	if err != nil {
		return Neutralization{}, err
	}
	note, fe := text(in.Note, "body.note", 0, maxNote)
	if fe != nil {
		return Neutralization{}, invalid(*fe)
	}
	if s.d.Reauth == nil {
		return Neutralization{}, ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		// The proof comes before the row is read: the record names the
		// neutralisation, not its subject.
		denied := audit.Event{Action: string(authz.TalentNeutralizationManage), ResourceType: authz.TypeTalentSubject,
			Metadata: map[string]any{"change": changeLifted, "neutralization_id": id.String()}}.By(a)
		return Neutralization{}, audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	var out Neutralization
	err = s.inTx(ctx, func(ctx context.Context) error {
		n, err := s.neutralizations().GetNeutralization(ctx, id)
		if err != nil {
			return err
		}
		if err := n.Liftable(); err != nil {
			return err
		}
		by := a.UserID
		row, lifted, err := s.neutralizations().LiftNeutralization(ctx, id, &by, s.now())
		switch {
		case err != nil:
			return err
		case !lifted:
			return ErrNotActive
		}
		out = row
		extra := map[string]any{audit.MetaReauth: kind}
		if note != "" {
			extra["note"] = note
		}
		return s.auditNeutralizationBy(ctx, a, row, changeLifted, reason, extra)
	})
	if err != nil {
		return Neutralization{}, err
	}
	return out, nil
}

// ReversePoint implements Admin. The reversal row negates the original's
// points and raw points with its occurred_at (the decay matches) and its
// config version; source_event_id is the reversal row's own ID (no event
// was consumed). The daily counters are not reduced (as a consumer's
// reversal). The original row never changes (append-only ledger).
func (s *service) ReversePoint(ctx context.Context, a authz.Actor, pointID uuid.UUID, in ReversalInput,
	proof identity.ReauthProof) (PointEvent, error) {
	reason, err := s.gate(ctx, a, authz.TalentReversalCreate, subject(nil), in.Reason)
	if err != nil {
		return PointEvent{}, err
	}
	why, fe := text(in.Why, "body.reason", 1, maxNote)
	if fe != nil {
		return PointEvent{}, invalid(*fe)
	}
	if s.d.Reauth == nil {
		return PointEvent{}, ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		denied := audit.Event{Action: string(authz.TalentReversalCreate), ResourceType: authz.TypeTalentSubject,
			Metadata: map[string]any{"point_event_id": pointID.String()}}.By(a)
		return PointEvent{}, audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	var out PointEvent
	err = s.inTx(ctx, func(ctx context.Context) error {
		orig, err := s.ledger().GetPoint(ctx, pointID)
		if err != nil {
			return err
		}
		if orig.ReversalOf != nil {
			return ErrNotReversible
		}
		id, of := uuid.NewV7(), orig.ID
		row, inserted, err := s.ledger().InsertPoint(ctx, PointEvent{ID: id, UserID: orig.UserID, Signal: orig.Signal,
			SourceType: SourceReversal, SourceID: orig.ID, OccurredAt: orig.OccurredAt, RawPointsCP: -orig.RawPointsCP,
			PointsCP: -orig.PointsCP, ReasonCode: ReasonAuditorReversal, ConfigVersionID: orig.ConfigVersionID,
			ReversalOf: &of, SourceEventID: id})
		switch {
		case err != nil:
			return err
		case !inserted:
			return ErrAlreadyReversed
		}
		out = row
		uid := orig.UserID
		return s.roles().AsApp(ctx, func(ctx context.Context) error {
			return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.TalentReversalCreate), Outcome: audit.Success,
				ResourceType: authz.TypeTalentSubject, ResourceID: &uid, Reason: reason,
				Metadata: map[string]any{"point_event_id": orig.ID.String(), "reversal_id": row.ID.String(),
					"note": why, audit.MetaReauth: kind}}.By(a))
		})
	})
	if err != nil {
		return PointEvent{}, err
	}
	return out, nil
}

// Status implements Admin: authz (talent_auditor; 403 for everyone else,
// before anything) → the flag. Side-effect free: no reason, no audit.
func (s *service) Status(ctx context.Context, a authz.Actor) (bool, error) {
	if err := authz.Can(ctx, a, authz.TalentConfigRead, adminSurface); err != nil {
		return false, err
	}
	return s.d.AuditorAPIEnabled, nil
}

// Config implements Admin.
func (s *service) Config(ctx context.Context, a authz.Actor, reason string) (ConfigView, error) {
	reason, err := s.gate(ctx, a, authz.TalentConfigRead, adminSurface, reason)
	if err != nil {
		return ConfigView{}, err
	}
	var out ConfigView
	err = s.inTx(ctx, func(ctx context.Context) error {
		var err error
		if out, err = s.configView(ctx); err != nil {
			return err
		}
		var res *uuid.UUID
		if out.Active != nil {
			id := out.Active.ID
			res = &id
		}
		return s.roles().AsApp(ctx, func(ctx context.Context) error {
			return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.TalentConfigRead), Outcome: audit.Success,
				ResourceType: resourceConfig, ResourceID: res, Reason: reason}.By(a))
		})
	})
	if err != nil {
		return ConfigView{}, err
	}
	return out, nil
}

// configView reads the version in force and the catalogue in the talent
// transaction of ctx.
func (s *service) configView(ctx context.Context) (ConfigView, error) {
	var out ConfigView
	switch v, err := s.configs().CurrentConfig(ctx, s.now()); {
	case err == nil:
		out.Active = &v
	case !errors.Is(err, ErrNotFound):
		return ConfigView{}, err
	}
	defs, err := s.configs().SignalDefs(ctx)
	if err != nil {
		return ConfigView{}, err
	}
	out.Signals = defs
	return out, nil
}

// CreateConfigVersion implements Admin. created_by is the auditor; the
// version is in force from now (effective_from, unique: a clash in the same
// microsecond is 409). Its params carry the snapshot of the whole
// catalogue after the updates.
func (s *service) CreateConfigVersion(ctx context.Context, a authz.Actor, in ConfigInput, proof identity.ReauthProof) (ConfigView, error) {
	reason, err := s.gate(ctx, a, authz.TalentConfigManage, adminSurface, in.Reason)
	if err != nil {
		return ConfigView{}, err
	}
	in.Params.Signals = nil // written below, never by the caller
	fe := in.Params.validate("body.params")
	fe = append(fe, validateSignalUpdates(in.Signals, "body.signal_defs")...)
	notes, nfe := text(in.Notes, "body.notes", 0, maxNote)
	if nfe != nil {
		fe = append(fe, *nfe)
	}
	if len(fe) > 0 {
		return ConfigView{}, invalid(fe...)
	}
	if s.d.Reauth == nil {
		return ConfigView{}, ErrUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		denied := audit.Event{Action: string(authz.TalentConfigManage), ResourceType: resourceConfig}.By(a)
		return ConfigView{}, audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	var out ConfigView
	err = s.inTx(ctx, func(ctx context.Context) error {
		st := s.configs()
		for i, u := range in.Signals {
			cur, err := st.SignalDef(ctx, u.Code)
			switch {
			case errors.Is(err, ErrNotFound):
				return invalid(problem.FieldError{Location: fmt.Sprintf("body.signal_defs[%d].code", i), Message: "unknown signal"})
			case err != nil:
				return err
			}
			cur.BasePointsCP, cur.DailyCapCP, cur.WeeklyCapCP, cur.HalfLifeDays, cur.Enabled =
				*u.BasePointsCP, u.DailyCapCP, u.WeeklyCapCP, u.HalfLifeDays, *u.Enabled
			if _, err := st.UpdateSignalDef(ctx, cur, a.UserID); err != nil {
				return err
			}
		}
		defs, err := st.SignalDefs(ctx)
		if err != nil {
			return err
		}
		p := in.Params
		for _, d := range defs {
			p.Signals = append(p.Signals, snapshot(d))
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return fmt.Errorf("talent: config params: %w", err)
		}
		v, err := st.InsertConfigVersion(ctx, ConfigVersion{ID: uuid.NewV7(), EffectiveFrom: s.now(), Params: raw,
			Notes: notes, CreatedBy: a.UserID})
		if err != nil {
			return err
		}
		out = ConfigView{Active: &v, Signals: defs}
		vid := v.ID
		return s.roles().AsApp(ctx, func(ctx context.Context) error {
			return s.d.Audit.Log(ctx, audit.Event{Action: string(authz.TalentConfigManage), Outcome: audit.Success,
				ResourceType: resourceConfig, ResourceID: &vid, Reason: reason,
				Metadata: map[string]any{"signals_changed": len(in.Signals), audit.MetaReauth: kind}}.By(a))
		})
	})
	if err != nil {
		return ConfigView{}, err
	}
	return out, nil
}

// ParamsOf decodes the params of a stored version for the auditor's view
// (a version written before BE-052 may not validate; it is shown as is).
func ParamsOf(v ConfigVersion) (ScoreParams, error) {
	var p ScoreParams
	if err := json.Unmarshal(v.Params, &p); err != nil {
		return ScoreParams{}, fmt.Errorf("talent: params of %s: %w", v.ID, err)
	}
	return p, nil
}
