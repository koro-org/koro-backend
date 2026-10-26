package talent

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/consent"
	"koro/internal/identity"
	"koro/internal/platform/httpx"
	"koro/internal/platform/httpx/op"
)

// closedNote is the part of every description about the flag.
const closedNote = "Closed (404 for every signed-in user) while talent.auditor_api_enabled is false; open, talent_auditor " +
	"only (MFA; never platform_admin). X-Koro-Access-Reason (10–500 characters) is mandatory and the call is audited " +
	"under its action with that reason. No name or e-mail is ever returned."

// Register adds the talent_auditor operations (04 §T; BE-052) to api. svc
// may be nil (`koro openapi`): handlers only touch it when called. Path
// IDs are plain strings and every input is checked by the service after
// the flag and authorisation, so a closed API answers 404 to every
// signed-in request that reaches it.
func Register(api huma.API, svc Admin) {
	h := handler{svc: svc}

	op.Register(api, "talent", huma.Operation{
		OperationID: "getTalentReport", Method: http.MethodGet, Path: "/admin/talent/subjects/{user_id}/report",
		Summary: "Get the talent report of a user",
		Description: "The ledger (with each row's point_event_id for a reversal), the category totals, the last nightly " +
			"score (band, raw score, cohort percentile, cohort, config version) and the neutralisations. " + closedNote,
	}, op.Meta{Action: string(authz.TalentReportRead), Surface: op.SurfaceAdmin}, h.report)

	op.Register(api, "talent", huma.Operation{
		OperationID: "getTalentDistribution", Method: http.MethodGet, Path: "/admin/talent/distribution",
		Summary: "Get the band distribution",
		Description: "Users by band 0..3 (neutralised users excluded); a count below 10 is null. university_id narrows it to " +
			"the cohorts of one university (a university below cohort_min is in the global cohort). " + closedNote,
	}, op.Meta{Action: string(authz.TalentDistributionRead), Surface: op.SurfaceAdmin}, h.distribution)

	op.Register(api, "talent", huma.Operation{
		OperationID: "createTalentNeutralization", Method: http.MethodPost, Path: "/admin/talent/subjects/{user_id}/neutralizations",
		DefaultStatus: http.StatusCreated,
		Summary:       "Neutralise a user's talent band",
		Description: "An auditor neutralisation (reason admin): the band is 0 while it is active. Re-authentication " +
			"required; one active auditor neutralisation per user (409 talent.already_neutralized). " + closedNote,
	}, op.Meta{Action: string(authz.TalentNeutralizationManage), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.createNeutralization)

	op.Register(api, "talent", huma.Operation{
		OperationID: "liftTalentNeutralization", Method: http.MethodPost, Path: "/admin/talent/neutralizations/{neutralization_id}/lift",
		Summary: "Lift an auditor neutralisation",
		Description: "Re-authentication required. An objection neutralisation is permanent (409 talent.objection_permanent), " +
			"an opt-out is lifted by the student's privacy setting only (409 talent.opt_out_student_owned). " + closedNote,
	}, op.Meta{Action: string(authz.TalentNeutralizationManage), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.liftNeutralization)

	op.Register(api, "talent", huma.Operation{
		OperationID: "reverseTalentPointEvent", Method: http.MethodPost, Path: "/admin/talent/point-events/{point_event_id}/reversal",
		DefaultStatus: http.StatusCreated,
		Summary:       "Reverse a talent ledger row",
		Description: "Appends the reversal row (points negated, the original's occurred_at and config version); the " +
			"original never changes. One reversal per row (409 talent.already_reversed); a reversal is not reversed " +
			"(409 talent.not_reversible). Re-authentication required. " + closedNote,
	}, op.Meta{Action: string(authz.TalentReversalCreate), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.reversePoint)

	op.Register(api, "talent", huma.Operation{
		OperationID: "getTalentConfig", Method: http.MethodGet, Path: "/admin/talent/config",
		Summary:     "Get the talent configuration",
		Description: "The config version in force (null before the first one) and the signal catalogue. " + closedNote,
	}, op.Meta{Action: string(authz.TalentConfigRead), Surface: op.SurfaceAdmin}, h.config)

	op.Register(api, "talent", huma.Operation{
		OperationID: "createTalentConfigVersion", Method: http.MethodPost, Path: "/admin/talent/config/versions",
		DefaultStatus: http.StatusCreated,
		Summary:       "Create a talent config version",
		Description: "Replaces the listed signal definitions and opens a new version in force from now: every ledger row " +
			"written from now on is frozen with it and the next nightly run scores with it. created_by is the auditor. " +
			"Re-authentication required. " + closedNote,
	}, op.Meta{Action: string(authz.TalentConfigManage), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth, Idempotency: op.IdempotencyOptional}, h.createConfigVersion)
}

type handler struct{ svc Admin }

func proofOf(r *consent.ReauthDTO) identity.ReauthProof {
	if r == nil {
		return identity.ReauthProof{}
	}
	return identity.ReauthProof{Password: r.Password, TOTPCode: r.TOTPCode}
}

// AccessReason is the mandatory justification header of every talent
// operation (03 §5.12; plain ASCII or RFC 8187 UTF-8, httpx.AccessReason).
type AccessReason struct {
	httpx.AccessReasonHeader
}

// SubjectPath is the subject of a talent operation.
type SubjectPath struct {
	UserID string `path:"user_id" doc:"The subject's user ID"`
}

type reportInput struct {
	SubjectPath
	AccessReason
}

type reportOutput struct{ Body TalentReportDTO }

func (h handler) report(ctx context.Context, in *reportInput) (*reportOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.svc.Report(ctx, a, httpx.ParseID(in.UserID), in.AccessReason.AccessReason.String())
	if err != nil {
		return nil, err
	}
	return &reportOutput{Body: reportDTO(r)}, nil
}

type distributionInput struct {
	AccessReason
	UniversityID string `query:"university_id" doc:"Only the cohorts of this university"`
}

type distributionOutput struct{ Body TalentDistributionDTO }

func (h handler) distribution(ctx context.Context, in *distributionInput) (*distributionOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.Distribution(ctx, a, DistributionQuery{Reason: in.AccessReason.AccessReason.String(), UniversityID: in.UniversityID})
	if err != nil {
		return nil, err
	}
	return &distributionOutput{Body: distributionDTO(d)}, nil
}

// NeutralizationBody is the body of the neutralisation operations.
type NeutralizationBody struct {
	Note   string             `json:"note,omitempty" doc:"Create: required (1–500 characters), stored with the neutralisation. Lift: optional, audited"`
	Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
}

type createNeutralizationInput struct {
	SubjectPath
	AccessReason
	Body NeutralizationBody
}

type neutralizationOutput struct{ Body TalentNeutralizationDTO }

func (h handler) createNeutralization(ctx context.Context, in *createNeutralizationInput) (*neutralizationOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	n, err := h.svc.CreateNeutralization(ctx, a, httpx.ParseID(in.UserID),
		NeutralizationInput{Reason: in.AccessReason.AccessReason.String(), Note: in.Body.Note}, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &neutralizationOutput{Body: neutralizationDTO(n)}, nil
}

type liftNeutralizationInput struct {
	NeutralizationID string `path:"neutralization_id"`
	AccessReason
	Body NeutralizationBody
}

func (h handler) liftNeutralization(ctx context.Context, in *liftNeutralizationInput) (*neutralizationOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	n, err := h.svc.LiftNeutralization(ctx, a, httpx.ParseID(in.NeutralizationID),
		NeutralizationInput{Reason: in.AccessReason.AccessReason.String(), Note: in.Body.Note}, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &neutralizationOutput{Body: neutralizationDTO(n)}, nil
}

type reversalInput struct {
	PointEventID string `path:"point_event_id"`
	AccessReason
	Body struct {
		Reason string             `json:"reason,omitempty" doc:"Required: why the row is wrong (1–500 characters; audited)"`
		Reauth *consent.ReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

type pointOutput struct{ Body TalentPointEventDTO }

func (h handler) reversePoint(ctx context.Context, in *reversalInput) (*pointOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ReversePoint(ctx, a, httpx.ParseID(in.PointEventID),
		ReversalInput{Reason: in.AccessReason.AccessReason.String(), Why: in.Body.Reason}, proofOf(in.Body.Reauth))
	if err != nil {
		return nil, err
	}
	return &pointOutput{Body: pointDTO(p, "")}, nil
}

type configInput struct {
	AccessReason
}

type configOutput struct{ Body TalentConfigDTO }

func (h handler) config(ctx context.Context, in *configInput) (*configOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	v, err := h.svc.Config(ctx, a, in.AccessReason.AccessReason.String())
	if err != nil {
		return nil, err
	}
	return &configOutput{Body: configDTO(v)}, nil
}

type createConfigVersionInput struct {
	AccessReason
	Body struct {
		Params     TalentParamsInputDTO      `json:"params,omitzero" doc:"Required: the score parameters (02 §15.1)"`
		SignalDefs []TalentSignalDefInputDTO `json:"signal_defs,omitempty" doc:"Signal definitions to replace (others stay)"`
		Notes      string                    `json:"notes,omitempty" doc:"Why the version changes (≤ 500 characters)"`
		Reauth     *consent.ReauthDTO        `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

func (h handler) createConfigVersion(ctx context.Context, in *createConfigVersionInput) (*configOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	v, err := h.svc.CreateConfigVersion(ctx, a, ConfigInput{Reason: in.AccessReason.AccessReason.String(), Params: paramsOf(b.Params),
		Signals: signalUpdatesOf(b.SignalDefs), Notes: b.Notes}, proofOf(b.Reauth))
	if err != nil {
		return nil, err
	}
	return &configOutput{Body: configDTO(v)}, nil
}
