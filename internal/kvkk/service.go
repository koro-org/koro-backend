package kvkk

import (
	"context"
	"encoding/base32"
	"encoding/binary"
	"log/slog"
	"net/mail"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"

	"koro/internal/audit"
	"koro/internal/authz"
	"koro/internal/platform/db"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// Topics (01 §6.2) and the outbox subscriptions of kvkk.
const (
	TopicSubmitted          = domainevent.TopicDSRSubmitted
	TopicErasureApproved    = domainevent.TopicDSRErasureApproved
	TopicProfilingObjection = domainevent.TopicDSRProfilingObjection
	TopicCompleted          = domainevent.TopicDSRCompleted

	// NotifyDPOTaskName handles dsr.submitted: the DPO mailbox is told.
	NotifyDPOTaskName = "kvkk.notify_dpo"
	// EraseAccountTaskName handles dsr.erasure_approved: the erasure run.
	EraseAccountTaskName = "kvkk.erase_account"
	// FinishRequestTaskName handles dsr.completed: export build (if any)
	// and the data subject's e-mail.
	FinishRequestTaskName = "kvkk.finish_request"
)

// Field limits (000088 CHECKs).
const (
	maxDetails   = 4000
	maxSummary   = 4000
	maxRejection = 1000
	maxName      = 200
	maxEmail     = 254
)

// MyRequest is the body of createMyDataRequest.
type MyRequest struct {
	Type    Type
	Details string
	Proof   ReauthProof // mandatory for erasure (03 k31); read for erasure only
}

// AdminRequest is the body of adminCreateDataRequest (post, KEP, e-mail or
// in-person application entered by the DPO).
type AdminRequest struct {
	UserID         *uuid.UUID
	RequesterName  string // only without UserID
	RequesterEmail string // mandatory without UserID
	Type           Type
	Channel        Channel
	Details        string
}

// Patch is the body of adminUpdateDataRequest; nil fields stay.
type Patch struct {
	Status             *Status // identity_check | in_progress
	AssignedTo         *uuid.UUID
	IdentityVerifiedAt *time.Time
	ResponseSummary    *string
}

// Stage is the step of adminApproveDataRequest.
type Stage string

// Approval stages (03 §6.1: B ≠ A).
const (
	StageFirst  Stage = "first"
	StageSecond Stage = "second"
)

// Download is a presigned export URL (04 §1.11).
type Download struct {
	URL       string
	ExpiresAt time.Time
}

// ETag is the strong ETag of a request (04 §1.13): the base32 of its
// updated_at in Unix nanoseconds, quoted.
func ETag(d DSR) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(d.UpdatedAt.UnixNano()))
	return `"` + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]) + `"`
}

// resource is the authz resource of a request (03 §3.5: type, status,
// first_approver; 06 H-27: OwnerUserID is the data subject).
func resource(d DSR, extra ...string) authz.Resource {
	first := ""
	if d.FirstApprover != nil {
		first = d.FirstApprover.String()
	}
	attrs := map[string]string{"type": string(d.Type), authz.AttrStatus: string(d.Status), authz.AttrFirstApprover: first}
	for i := 0; i+1 < len(extra); i += 2 {
		attrs[extra[i]] = extra[i+1]
	}
	return authz.Resource{Type: authz.TypeDSR, ID: d.ID, OwnerUserID: d.UserID, Attrs: attrs}
}

// ownResource is the resource of a /me operation before any row is read.
func ownResource(a authz.Actor, t Type) authz.Resource {
	me := a.UserID
	return authz.Resource{Type: authz.TypeDSR, OwnerUserID: &me,
		Attrs: map[string]string{"type": string(t), authz.AttrStatus: "", authz.AttrFirstApprover: ""}}
}

// adminSurface is the resource of the admin pre-check (03 §7.2-3: the
// surface answers 403 before anything is read).
var adminSurface = authz.Resource{Type: authz.TypeAdminSurface, Attrs: map[string]string{authz.AttrFirstApprover: ""}}

func invalid(location, msg string) error {
	return ErrInvalidRequest.WithFields(problem.FieldError{Location: location, Message: msg})
}

// cleanText checks free text: valid UTF-8, at most max runes, no control
// characters but tab and newline, no bidirectional overrides. The value is
// never echoed.
func cleanText(raw, location string, maxRunes int, required bool) (string, error) {
	s := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(raw, "\r\n", "\n"), "\r", "\n"))
	switch {
	case !utf8.ValidString(s):
		return "", invalid(location, "must be valid UTF-8")
	case required && s == "":
		return "", invalid(location, "is required")
	case utf8.RuneCountInString(s) > maxRunes:
		return "", invalid(location, "is too long")
	}
	for _, r := range s {
		if (unicode.IsControl(r) && r != '\n' && r != '\t') || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return "", invalid(location, "must be printable text")
		}
	}
	return s, nil
}

// event builds the audit row of a step on d.
func event(a authz.Actor, act authz.Action, d DSR, meta map[string]any) audit.Event {
	id := d.ID
	m := map[string]any{"type": string(d.Type), "status": string(d.Status)}
	for k, v := range meta {
		m[k] = v
	}
	if d.UserID != nil && (a.Kind != authz.KindUser || *d.UserID != a.UserID) {
		m[audit.MetaSubjectUserID] = d.UserID.String()
	}
	return audit.Event{Action: string(act), Outcome: audit.Success, ResourceType: authz.TypeDSR, ResourceID: &id, Metadata: m}.By(a)
}

// emit writes an outbox event in the transaction carried by ctx.
func (s *service) emit(ctx context.Context, p domainevent.Payload, id uuid.UUID) error {
	ev, err := domainevent.New(p, "dsr", id)
	if err != nil {
		return err
	}
	return s.d.Outbox.Add(ctx, ev)
}

// reauth verifies the R proof of act on a resource (03 §6.2) and returns
// its kind for the audit row. Only a verdict is audited: a refused proof
// (a 4xx: reauth_required, reauth_failed, rate_limited) is denied with
// reason reauth_failed. An error without a verdict — the request budget
// ran out before identity could check the proof (BE-070/071), or a
// dependency failed — writes nothing and goes back unchanged; httpx
// answers 503 (WARN) or 500 and logs it once (BE-072; audit.ReauthFailed,
// BE-074).
func (s *service) reauth(ctx context.Context, a authz.Actor, act authz.Action, resType string, resID *uuid.UUID, proof ReauthProof) (string, error) {
	if s.d.Reauth == nil {
		return "", ErrReauthUnavailable
	}
	kind, err := s.d.Reauth.Verify(ctx, a.UserID, proof)
	if err != nil {
		denied := audit.Event{Action: string(act), ResourceType: resType, ResourceID: resID}.By(a)
		return "", audit.ReauthFailed(ctx, s.d.Audit, s.d.Log, denied, err)
	}
	return kind, nil
}

func (s *service) logStep(ctx context.Context, msg string, d DSR) {
	s.d.Log.LogAttrs(ctx, slog.LevelInfo, msg, slog.String("dsr_id", d.ID.String()),
		slog.String("type", string(d.Type)), slog.String("status", string(d.Status)))
}

// ---------------------------------------------------------------- data subject

// CreateMine implements Service (createMyDataRequest).
func (s *service) CreateMine(ctx context.Context, a authz.Actor, in MyRequest) (DSR, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrCreate, ownResource(a, in.Type)); err != nil {
		return DSR{}, err
	}
	if !in.Type.Valid() {
		return DSR{}, invalid("body.type", "unknown request type")
	}
	details, err := cleanText(in.Details, "body.details", maxDetails, false)
	if err != nil {
		return DSR{}, err
	}
	now := s.now()
	d := DSR{ID: uuid.NewV7(), Type: in.Type, Channel: ChannelInApp, Status: StatusReceived, Details: details,
		ReceivedAt: now, DueAt: now.Add(LegalPeriod)}
	me := a.UserID
	d.UserID = &me
	meta := map[string]any{"channel": string(ChannelInApp)}
	if in.Type == TypeErasure {
		// 03 k31: the erasure request itself is the R step; the
		// re-authentication moment is the identity check (02 §16.1).
		kind, err := s.reauth(ctx, a, authz.KvkkDsrCreate, authz.TypeDSR, nil, in.Proof)
		if err != nil {
			return DSR{}, err
		}
		meta[audit.MetaReauth] = kind
	}
	// An in-app request comes from the authenticated account holder: the
	// session (and for erasure the proof just verified) is the identity
	// check.
	verified := s.now()
	d.IdentityVerifiedAt = &verified

	hash, err := idempotency.Hash(struct {
		User    uuid.UUID
		Type    Type
		Details string
	}{me, in.Type, details})
	if err != nil {
		return DSR{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (DSR, error) {
		open, err := s.d.Store.HasOpen(ctx, me, in.Type)
		if err != nil {
			return DSR{}, err
		}
		if open {
			return DSR{}, ErrDuplicateRequest
		}
		if err := s.d.Store.Insert(ctx, d); err != nil {
			return DSR{}, err
		}
		if err := s.d.Audit.Log(ctx, event(a, authz.KvkkDsrCreate, d, meta)); err != nil {
			return DSR{}, err
		}
		if err := s.emit(ctx, domainevent.DSRSubmitted{DSRID: d.ID, Type: string(d.Type)}, d.ID); err != nil {
			return DSR{}, err
		}
		return s.d.Store.Get(ctx, d.ID)
	})
	if err != nil {
		return DSR{}, err
	}
	s.logStep(ctx, "data subject request received", out)
	return out, nil
}

// ListMine implements Service (listMyDataRequests).
func (s *service) ListMine(ctx context.Context, a authz.Actor, after *After, limit int) (Page, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrListOwn, ownResource(a, "")); err != nil {
		return Page{}, err
	}
	limit = page.ClampLimit(limit)
	rows, err := s.d.Store.ListOwn(ctx, a.UserID, after, limit+1)
	if err != nil {
		return Page{}, err
	}
	items, more := page.Cut(rows, limit)
	p := Page{Items: items}
	if more {
		last := items[len(items)-1]
		p.Next = &After{At: last.ReceivedAt, ID: last.ID}
	}
	return p, nil
}

// mine reads the caller's request; another user's is not found (IDOR).
func (s *service) mine(ctx context.Context, a authz.Actor, id uuid.UUID) (DSR, error) {
	if !a.IsUser() {
		return DSR{}, problem.ErrUnauthenticated
	}
	return s.d.Store.GetOwn(ctx, id, a.UserID)
}

// GetMine implements Service (getMyDataRequest).
func (s *service) GetMine(ctx context.Context, a authz.Actor, id uuid.UUID) (DSR, error) {
	d, err := s.mine(ctx, a, id)
	if err != nil {
		return DSR{}, err
	}
	if err := authz.Can(ctx, a, authz.KvkkDsrReadOwn, resource(d)); err != nil {
		return DSR{}, err
	}
	return d, nil
}

// CancelMine implements Service (cancelMyDataRequest): only before the DPO
// started working on it (received, identity_check; 03 §5.22).
func (s *service) CancelMine(ctx context.Context, a authz.Actor, id uuid.UUID) (DSR, error) {
	if _, err := s.mine(ctx, a, id); err != nil {
		return DSR{}, err
	}
	var out DSR
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		d, err := s.d.Store.Lock(ctx, id)
		if err != nil {
			return err
		}
		if err := authz.Can(ctx, a, authz.KvkkDsrCancelOwn, resource(d)); err != nil {
			return err
		}
		if d.Status != StatusReceived && d.Status != StatusIdentityCheck {
			return ErrState
		}
		d.Status = StatusCancelled
		if err := s.d.Store.Update(ctx, d); err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, event(a, authz.KvkkDsrCancelOwn, d, nil)); err != nil {
			return err
		}
		out, err = s.d.Store.Get(ctx, id)
		return err
	})
	if err != nil {
		return DSR{}, err
	}
	s.logStep(ctx, "data subject request cancelled", out)
	return out, nil
}

// DownloadMine implements Service (downloadMyDataExport).
func (s *service) DownloadMine(ctx context.Context, a authz.Actor, id uuid.UUID) (Download, error) {
	d, err := s.mine(ctx, a, id)
	if err != nil {
		return Download{}, err
	}
	if err := authz.Can(ctx, a, authz.KvkkDsrExportDownload, resource(d)); err != nil {
		return Download{}, err
	}
	return s.download(ctx, a, d)
}

// ---------------------------------------------------------------- DPO

// staffOnly reports whether a holds platform_admin but not dpo: such an
// actor sees only requests awaiting the second approval (03 k32).
func staffOnly(a authz.Actor) bool {
	return a.HasPlatformRole(authz.PlatformAdmin) && !a.HasPlatformRole(authz.PlatformDPO)
}

// AdminList implements Service (adminListDataRequests).
func (s *service) AdminList(ctx context.Context, a authz.Actor, f AdminFilter, after *After, limit int) (Page, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrList, adminSurface); err != nil {
		return Page{}, err
	}
	if staffOnly(a) {
		f.Statuses = []Status{StatusAwaitingSecond}
	}
	limit = page.ClampLimit(limit)
	rows, err := s.d.Store.List(ctx, f, after, limit+1)
	if err != nil {
		return Page{}, err
	}
	items, more := page.Cut(rows, limit)
	p := Page{Items: items}
	if more {
		last := items[len(items)-1]
		p.Next = &After{At: last.DueAt, ID: last.ID}
	}
	return p, nil
}

// AdminGet implements Service (adminGetDataRequest).
func (s *service) AdminGet(ctx context.Context, a authz.Actor, id uuid.UUID) (DSR, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrRead, adminSurface); err != nil {
		return DSR{}, err
	}
	d, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return DSR{}, err
	}
	if err := authz.Can(ctx, a, authz.KvkkDsrRead, resource(d)); err != nil {
		return DSR{}, err
	}
	return d, nil
}

// AdminCreate implements Service (adminCreateDataRequest).
func (s *service) AdminCreate(ctx context.Context, a authz.Actor, in AdminRequest) (DSR, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrCreateAdmin, adminSurface); err != nil {
		return DSR{}, err
	}
	switch {
	case !in.Type.Valid():
		return DSR{}, invalid("body.type", "unknown request type")
	case !in.Channel.Valid() || in.Channel == ChannelInApp:
		return DSR{}, invalid("body.channel", "must be email, post, kep or in_person")
	}
	details, err := cleanText(in.Details, "body.details", maxDetails, false)
	if err != nil {
		return DSR{}, err
	}
	now := s.now()
	d := DSR{ID: uuid.NewV7(), Type: in.Type, Channel: in.Channel, Status: StatusReceived, Details: details,
		ReceivedAt: now, DueAt: now.Add(LegalPeriod)}
	if in.UserID != nil {
		if in.RequesterName != "" || in.RequesterEmail != "" {
			return DSR{}, invalid("body.requester_email", "only for applicants without an account")
		}
		if *in.UserID == a.UserID {
			return DSR{}, ErrOwnRequest
		}
		ok, err := s.d.Users.Exists(ctx, *in.UserID)
		if err != nil {
			return DSR{}, err
		}
		if !ok {
			return DSR{}, ErrUnknownUser
		}
		uid := *in.UserID
		d.UserID = &uid
	} else {
		name, err := cleanText(in.RequesterName, "body.requester_name", maxName, false)
		if err != nil {
			return DSR{}, err
		}
		email := strings.TrimSpace(in.RequesterEmail)
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email || len(email) > maxEmail {
			return DSR{}, invalid("body.requester_email", "must be a bare e-mail address (mandatory without user_id)")
		}
		d.RequesterName, d.RequesterEmail = name, email
	}
	hash, err := idempotency.Hash(in)
	if err != nil {
		return DSR{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (DSR, error) {
		if err := s.d.Store.Insert(ctx, d); err != nil {
			return DSR{}, err
		}
		if err := s.d.Audit.Log(ctx, event(a, authz.KvkkDsrCreateAdmin, d, map[string]any{"channel": string(d.Channel)})); err != nil {
			return DSR{}, err
		}
		return s.d.Store.Get(ctx, d.ID)
	})
	if err != nil {
		return DSR{}, err
	}
	s.logStep(ctx, "data subject request entered", out)
	return out, nil
}

// handle runs step on the locked request id after the admin pre-check:
// the subject's own request is refused, then authz decides on the loaded
// row (extra Attrs: approve's stage).
func (s *service) handle(ctx context.Context, a authz.Actor, act authz.Action, id uuid.UUID, extra []string,
	step func(ctx context.Context, d *DSR) (map[string]any, error),
) (DSR, error) {
	var out DSR
	err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		d, err := s.d.Store.Lock(ctx, id)
		if err != nil {
			return err
		}
		if err := authz.Can(ctx, a, act, resource(d, extra...)); err != nil {
			return err
		}
		if d.UserID != nil && *d.UserID == a.UserID {
			return ErrOwnRequest
		}
		meta, err := step(ctx, &d)
		if err != nil {
			return err
		}
		if err := s.d.Store.Update(ctx, d); err != nil {
			return err
		}
		if err := s.d.Audit.Log(ctx, event(a, act, d, meta)); err != nil {
			return err
		}
		out, err = s.d.Store.Get(ctx, id)
		return err
	})
	return out, err
}

// AdminUpdate implements Service (adminUpdateDataRequest): assignment,
// identity check, working status and the draft answer.
func (s *service) AdminUpdate(ctx context.Context, a authz.Actor, id uuid.UUID, ifMatch string, p Patch) (DSR, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrUpdate, adminSurface); err != nil {
		return DSR{}, err
	}
	if p == (Patch{}) {
		return DSR{}, invalid("body", "nothing to update")
	}
	if ifMatch == "" {
		return DSR{}, problem.ErrPreconditionRequired
	}
	var summary string
	if p.ResponseSummary != nil {
		var err error
		if summary, err = cleanText(*p.ResponseSummary, "body.response_summary", maxSummary, false); err != nil {
			return DSR{}, err
		}
	}
	if p.AssignedTo != nil {
		ok, err := s.d.Users.Exists(ctx, *p.AssignedTo)
		if err != nil {
			return DSR{}, err
		}
		if !ok {
			return DSR{}, ErrUnknownUser
		}
	}
	out, err := s.handle(ctx, a, authz.KvkkDsrUpdate, id, nil, func(_ context.Context, d *DSR) (map[string]any, error) {
		if ETag(*d) != strings.TrimPrefix(ifMatch, "W/") {
			return nil, problem.ErrPreconditionFailed
		}
		if !d.Status.workable() {
			return nil, ErrState
		}
		var changed []string
		if p.Status != nil {
			if *p.Status != StatusIdentityCheck && *p.Status != StatusInProgress {
				return nil, invalid("body.status", "must be identity_check or in_progress")
			}
			d.Status = *p.Status
			changed = append(changed, "status")
		}
		if p.AssignedTo != nil {
			v := *p.AssignedTo
			d.AssignedTo = &v
			changed = append(changed, "assigned_to")
		}
		if p.IdentityVerifiedAt != nil {
			at := p.IdentityVerifiedAt.UTC().Truncate(time.Microsecond)
			if at.After(s.now()) || at.Before(d.ReceivedAt.Add(-LegalPeriod)) {
				return nil, invalid("body.identity_verified_at", "must lie between 30 days before receipt and now")
			}
			d.IdentityVerifiedAt = &at
			changed = append(changed, "identity_verified_at")
		}
		if p.ResponseSummary != nil {
			d.ResponseSummary = summary
			changed = append(changed, "response_summary")
		}
		return map[string]any{"changed": strings.Join(changed, ",")}, nil
	})
	if err != nil {
		return DSR{}, err
	}
	s.logStep(ctx, "data subject request updated", out)
	return out, nil
}

// Approve implements Service (adminApproveDataRequest): the two approvals
// of a profiling report (03 §6.1). The four-eyes rule is authz's
// (first_approver ≠ actor) and the 000088 CHECK's.
func (s *service) Approve(ctx context.Context, a authz.Actor, id uuid.UUID, stage Stage) (DSR, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrApprove, adminSurface); err != nil {
		return DSR{}, err
	}
	if stage != StageFirst && stage != StageSecond {
		return DSR{}, invalid("body.stage", "must be first or second")
	}
	out, err := s.handle(ctx, a, authz.KvkkDsrApprove, id, []string{"stage", string(stage)},
		func(_ context.Context, d *DSR) (map[string]any, error) {
			if d.Type != TypeProfilingAccess {
				return nil, ErrApprovalNotApplicable
			}
			me := a.UserID
			switch {
			case stage == StageFirst && d.Status.workable() && d.FirstApprover == nil:
				d.FirstApprover, d.Status = &me, StatusAwaitingSecond
			case stage == StageSecond && d.Status == StatusAwaitingSecond && d.FirstApprover != nil && d.SecondApprover == nil:
				d.SecondApprover, d.Status = &me, StatusInProgress
			default:
				return nil, ErrState
			}
			return map[string]any{"stage": string(stage)}, nil
		})
	if err != nil {
		return DSR{}, err
	}
	s.logStep(ctx, "data subject request approved", out)
	return out, nil
}

// Complete implements Service (adminCompleteDataRequest, R): the DPO's
// answer. The request is closed now (the legal answer); what the answer
// sets in motion runs in the worker:
//
//	erasure                dsr.erasure_approved → erasure run → dsr.completed
//	access, export,        dsr.completed → export zip → e-mail to the subject
//	profiling_access
//	profiling_objection    dsr.profiling_objection (talent) + dsr.completed
//	others                 dsr.completed → e-mail to the subject
func (s *service) Complete(ctx context.Context, a authz.Actor, id uuid.UUID, summary string, proof ReauthProof) (DSR, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrComplete, adminSurface); err != nil {
		return DSR{}, err
	}
	kind, err := s.reauth(ctx, a, authz.KvkkDsrComplete, authz.TypeDSR, &id, proof)
	if err != nil {
		return DSR{}, err
	}
	answer, err := cleanText(summary, "body.response_summary", maxSummary, true)
	if err != nil {
		return DSR{}, err
	}
	hash, err := idempotency.Hash(struct {
		ID      uuid.UUID
		Summary string
	}{id, answer})
	if err != nil {
		return DSR{}, err
	}
	out, _, err := idempotency.Do(ctx, s.d.Idempotency, hash, func(ctx context.Context) (DSR, error) {
		return s.handle(ctx, a, authz.KvkkDsrComplete, id, nil, func(ctx context.Context, d *DSR) (map[string]any, error) {
			switch {
			case !d.Status.workable():
				return nil, ErrState
			case d.IdentityVerifiedAt == nil:
				return nil, ErrIdentityNotVerified
			case d.Type == TypeProfilingAccess && d.SecondApprover == nil:
				return nil, ErrSecondApprovalRequired
			}
			now := s.now()
			d.Status, d.CompletedAt, d.ResponseSummary = StatusCompleted, &now, answer
			outcome := s.outcome(*d)
			subject := uuid.Nil()
			if d.UserID != nil {
				subject = *d.UserID
			}
			var err error
			switch {
			case outcome == OutcomeErased:
				err = s.emit(ctx, domainevent.DSRErasureApproved{DSRID: d.ID, UserID: subject}, d.ID)
				// 06 H-49: sign the subject out as soon as the erasure is
				// approved, after the commit (not a same-transaction write).
				db.AfterCommit(ctx, func(ctx context.Context) { s.revokeSessions(ctx, d.ID, subject) })
			case d.Type == TypeProfilingObjection && d.UserID != nil:
				err = s.emit(ctx, domainevent.DSRProfilingObjection{DSRID: d.ID, UserID: subject}, d.ID)
			}
			if err != nil {
				return nil, err
			}
			if outcome != OutcomeErased { // the erasure run emits dsr.completed when it is done
				if err := s.emit(ctx, domainevent.DSRCompleted{DSRID: d.ID, UserID: subject, Outcome: outcome}, d.ID); err != nil {
					return nil, err
				}
			}
			return map[string]any{audit.MetaReauth: kind, "outcome": outcome}, nil
		})
	})
	if err != nil {
		return DSR{}, err
	}
	s.logStep(ctx, "data subject request completed", out)
	return out, nil
}

// outcome is what completing d sets in motion.
func (s *service) outcome(d DSR) string {
	switch {
	case d.UserID == nil && (d.Type == TypeErasure || d.Type.exports()):
		return OutcomeNoAccount
	case d.Type == TypeErasure:
		return OutcomeErased
	case d.Type.exports():
		return OutcomeExport
	}
	return OutcomeAnswered
}

// Reject implements Service (adminRejectDataRequest): a reasoned refusal
// (KVKK md. 13) closes the request; the subject is told.
func (s *service) Reject(ctx context.Context, a authz.Actor, id uuid.UUID, reason string) (DSR, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrReject, adminSurface); err != nil {
		return DSR{}, err
	}
	why, err := cleanText(reason, "body.rejection_reason", maxRejection, true)
	if err != nil {
		return DSR{}, err
	}
	out, err := s.handle(ctx, a, authz.KvkkDsrReject, id, nil, func(ctx context.Context, d *DSR) (map[string]any, error) {
		if !d.Status.Open() {
			return nil, ErrState
		}
		now := s.now()
		d.Status, d.CompletedAt, d.RejectionReason = StatusRejected, &now, why
		subject := uuid.Nil()
		if d.UserID != nil {
			subject = *d.UserID
		}
		if err := s.emit(ctx, domainevent.DSRCompleted{DSRID: d.ID, UserID: subject, Outcome: OutcomeRejected}, d.ID); err != nil {
			return nil, err
		}
		return nil, nil
	})
	if err != nil {
		return DSR{}, err
	}
	s.logStep(ctx, "data subject request rejected", out)
	return out, nil
}

// AdminDownload implements Service (adminDownloadDataExport).
func (s *service) AdminDownload(ctx context.Context, a authz.Actor, id uuid.UUID) (Download, error) {
	if err := authz.Can(ctx, a, authz.KvkkDsrExportDownload, adminSurface); err != nil {
		return Download{}, err
	}
	d, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Download{}, err
	}
	if err := authz.Can(ctx, a, authz.KvkkDsrExportDownload, resource(d)); err != nil {
		return Download{}, err
	}
	return s.download(ctx, a, d)
}
