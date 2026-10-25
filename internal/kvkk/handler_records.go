package kvkk

import (
	"context"
	"net/http"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/authz"
	"koro/internal/platform/civil"
	"koro/internal/platform/httpx/op"
	"koro/internal/platform/idempotency"
	"koro/internal/platform/page"
	"koro/internal/platform/problem"
)

// registerRecords adds the 9 kvkk-2 operations (04 §2.22; BE-049).
func registerRecords(api huma.API, h handler) {
	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminListRetentionPolicies", Method: http.MethodGet, Path: "/admin/retention-policies",
		Summary: "List the retention policies",
		Description: "Every policy with its period (ISO 8601), anchor, method, legal approval and whether a sweeper is " +
			"registered for it. A policy that is not legally approved runs as a dry run only (count, no deletion). " +
			"Each item carries the etag that adminUpdateRetentionPolicy expects in If-Match.",
	}, op.Meta{Action: string(authz.KvkkRetentionPolicyList), Surface: op.SurfaceAdmin}, h.listPolicies)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminUpdateRetentionPolicy", Method: http.MethodPatch, Path: "/admin/retention-policies/{code}",
		Summary: "Change a retention period, method, state or legal approval",
		Description: "Re-authentication required; If-Match (428 without, 412 when stale). Changing the period or the " +
			"method withdraws the legal approval unless the same request approves the new values with approved_ref. " +
			"The data export files live at most 7 days.",
	}, op.Meta{Action: string(authz.KvkkRetentionPolicyUpdate), Surface: op.SurfaceAdmin, Reauth: true,
		RateLimit: op.RateReauth}, h.updatePolicy)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminListDestructionLogs", Method: http.MethodGet, Path: "/admin/destruction-logs",
		Summary:     "List the destruction record",
		Description: "One row per retention policy and nightly run, and per module of an account erasure; newest first.",
	}, op.Meta{Action: string(authz.KvkkDestructionLogList), Surface: op.SurfaceAdmin}, h.listLogs)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminListBreaches", Method: http.MethodGet, Path: "/admin/breaches",
		Summary:     "List the personal data breaches",
		Description: "With the 72-hour deadline of the Board notification and the 24-hour notice to the data controller.",
	}, op.Meta{Action: string(authz.KvkkBreachList), Surface: op.SurfaceAdmin}, h.listBreaches)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminCreateBreach", Method: http.MethodPost, Path: "/admin/breaches",
		Summary: "Record a personal data breach", DefaultStatus: http.StatusCreated,
		Description: "authority_deadline_at = detected_at + 72 hours. The DPO and the platform admins are alerted at once " +
			"and again at 48/24/6 hours left. Do not put a person's name in the title or the description.",
	}, op.Meta{Action: string(authz.KvkkBreachManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.createBreach)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminUpdateBreach", Method: http.MethodPatch, Path: "/admin/breaches/{breach_id}",
		Summary: "Update a breach: containment, notifications, closing",
		Description: "If-Match. The status only moves forward (open → contained → notified → closed); notified needs " +
			"authority_notified_at; notification times are written once; detected_at may only move earlier.",
	}, op.Meta{Action: string(authz.KvkkBreachManage), Surface: op.SurfaceAdmin}, h.updateBreach)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminListSubprocessors", Method: http.MethodGet, Path: "/admin/subprocessors",
		Summary: "List the sub-processor inventory",
	}, op.Meta{Action: string(authz.KvkkSubprocessorList), Surface: op.SurfaceAdmin}, h.listSubs)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminCreateSubprocessor", Method: http.MethodPost, Path: "/admin/subprocessors",
		Summary: "Add a sub-processor", DefaultStatus: http.StatusCreated,
		Description: "A processor abroad is never domestic (KVKK md. 9). 409 kvkk.duplicate_subprocessor on a taken name.",
	}, op.Meta{Action: string(authz.KvkkSubprocessorManage), Surface: op.SurfaceAdmin, Idempotency: op.IdempotencyOptional}, h.createSub)

	op.Register(api, "kvkk", huma.Operation{
		OperationID: "adminUpdateSubprocessor", Method: http.MethodPatch, Path: "/admin/subprocessors/{subprocessor_id}",
		Summary: "Update a sub-processor", Description: "If-Match (the etag of the list item). Retire with active = false.",
	}, op.Meta{Action: string(authz.KvkkSubprocessorManage), Surface: op.SurfaceAdmin}, h.updateSub)
}

// ---- DTOs

// RetentionPolicyDTO is a retention policy (02 §16.2).
type RetentionPolicyDTO struct {
	Code              string    `json:"code"`
	OwnerModule       string    `json:"owner_module"`
	Target            string    `json:"target"`
	Period            string    `json:"period" doc:"ISO 8601 duration (P3Y, P12M, P30D, PT24H, P0D)"`
	Anchor            string    `json:"anchor"`
	Method            string    `json:"method" enum:"hard_delete,anonymize,null_columns,file_delete"`
	LegalBasis        string    `json:"legal_basis"`
	LegalApproved     bool      `json:"legal_approved" doc:"false: the sweeper only counts (dry run)"`
	ApprovedRef       string    `json:"approved_ref,omitempty"`
	Enabled           bool      `json:"enabled"`
	DescriptionTR     string    `json:"description_tr"`
	SweeperRegistered bool      `json:"sweeper_registered" doc:"A module sweeper runs this policy nightly"`
	UpdatedBy         string    `json:"updated_by,omitempty" format:"uuid"`
	UpdatedAt         time.Time `json:"updated_at"`
	ETag              string    `json:"etag" doc:"If-Match of adminUpdateRetentionPolicy"`
}

func toPolicyDTO(p PolicyView) RetentionPolicyDTO {
	return RetentionPolicyDTO{Code: p.Code, OwnerModule: p.OwnerModule, Target: p.Target, Period: p.Period.String(),
		Anchor: p.Anchor, Method: p.Method, LegalBasis: p.LegalBasis, LegalApproved: p.LegalApproved, ApprovedRef: p.ApprovedRef,
		Enabled: p.Enabled, DescriptionTR: p.DescriptionTR, SweeperRegistered: p.SweeperRegistered,
		UpdatedBy: idText(p.UpdatedBy), UpdatedAt: p.UpdatedAt, ETag: ETagOf(p.UpdatedAt)}
}

// DestructionLogDTO is one destruction record row (02 §16.3).
type DestructionLogDTO struct {
	DestructionLogID string         `json:"destruction_log_id" format:"uuid"`
	PolicyCode       string         `json:"policy_code"`
	RunID            string         `json:"run_id" format:"uuid" doc:"The nightly run; for dsr_erasure the request ID"`
	Module           string         `json:"module"`
	Target           string         `json:"target"`
	Method           string         `json:"method" enum:"hard_delete,anonymize,null_columns,file_delete"`
	RecordCount      int            `json:"record_count" doc:"Records destroyed (0 on a dry run)"`
	PeriodCutoff     time.Time      `json:"period_cutoff"`
	StartedAt        time.Time      `json:"started_at"`
	FinishedAt       time.Time      `json:"finished_at"`
	Details          map[string]any `json:"details" doc:"Counts and errors only (dry_run, eligible, error, ...)"`
	CreatedAt        time.Time      `json:"created_at"`
}

func toLogDTO(l DestructionLog) DestructionLogDTO {
	d := l.Details
	if d == nil {
		d = map[string]any{}
	}
	return DestructionLogDTO{DestructionLogID: l.ID.String(), PolicyCode: l.PolicyCode, RunID: l.RunID.String(),
		Module: l.Module, Target: l.Target, Method: l.Method, RecordCount: l.RecordCount, PeriodCutoff: l.PeriodCutoff,
		StartedAt: l.StartedAt, FinishedAt: l.FinishedAt, Details: d, CreatedAt: l.CreatedAt}
}

// BreachDTO is a breach (02 §16.4).
type BreachDTO struct {
	BreachID             string     `json:"breach_id" format:"uuid"`
	Title                string     `json:"title"`
	DetectedAt           time.Time  `json:"detected_at"`
	AuthorityDeadlineAt  time.Time  `json:"authority_deadline_at" doc:"detected_at + 72 hours (Board notification)"`
	ControllerDeadlineAt time.Time  `json:"controller_deadline_at" doc:"detected_at + 24 hours (notice to the data controller)"`
	AuthorityHoursLeft   *int       `json:"authority_hours_left,omitempty" doc:"Whole hours to the Board deadline while it is open (negative: overdue)"`
	ControllerNotifiedAt *time.Time `json:"controller_notified_at,omitempty"`
	AuthorityNotifiedAt  *time.Time `json:"authority_notified_at,omitempty"`
	DataCategories       []string   `json:"data_categories"`
	AffectedEstimate     *int       `json:"affected_estimate,omitempty"`
	Description          string     `json:"description"`
	Status               string     `json:"status" enum:"open,contained,notified,closed"`
	OwnerUserID          string     `json:"owner_user_id" format:"uuid"`
	ClosedAt             *time.Time `json:"closed_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	ETag                 string     `json:"etag" doc:"If-Match of adminUpdateBreach"`
}

func toBreachDTO(b Breach, now time.Time) BreachDTO {
	d := BreachDTO{BreachID: b.ID.String(), Title: b.Title, DetectedAt: b.DetectedAt, AuthorityDeadlineAt: b.AuthorityDeadlineAt,
		ControllerDeadlineAt: b.ControllerDeadlineAt(), ControllerNotifiedAt: b.ControllerNotifiedAt,
		AuthorityNotifiedAt: b.AuthorityNotifiedAt, DataCategories: b.DataCategories, AffectedEstimate: b.AffectedEstimate,
		Description: b.Description, Status: string(b.Status), OwnerUserID: b.OwnerUserID.String(), ClosedAt: b.ClosedAt,
		CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, ETag: ETagOf(b.UpdatedAt)}
	if d.DataCategories == nil {
		d.DataCategories = []string{}
	}
	if b.AuthorityNotifiedAt == nil && b.Status != BreachClosed {
		h := int(b.AuthorityDeadlineAt.Sub(now).Truncate(time.Hour) / time.Hour)
		d.AuthorityHoursLeft = &h
	}
	return d
}

// SubprocessorDTO is a sub-processor (02 §16.5).
type SubprocessorDTO struct {
	SubprocessorID      string    `json:"subprocessor_id" format:"uuid"`
	Name                string    `json:"name"`
	Service             string    `json:"service"`
	DataCategories      []string  `json:"data_categories"`
	Region              string    `json:"region"`
	CountryCode         string    `json:"country_code"`
	TransferMechanism   string    `json:"transfer_mechanism" enum:"domestic,standard_contract,adequacy,explicit_consent,undecided"`
	ContractSignedAt    string    `json:"contract_signed_at,omitempty" format:"date"`
	AuthorityNotifiedAt string    `json:"authority_notified_at,omitempty" format:"date"`
	Active              bool      `json:"active"`
	Notes               string    `json:"notes,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	ETag                string    `json:"etag" doc:"If-Match of adminUpdateSubprocessor"`
}

func dateText(d *civil.Date) string {
	if d == nil {
		return ""
	}
	return d.String()
}

func toSubDTO(x Subprocessor) SubprocessorDTO {
	d := SubprocessorDTO{SubprocessorID: x.ID.String(), Name: x.Name, Service: x.Service, DataCategories: x.DataCategories,
		Region: x.Region, CountryCode: x.CountryCode, TransferMechanism: x.TransferMechanism,
		ContractSignedAt: dateText(x.ContractSignedAt), AuthorityNotifiedAt: dateText(x.AuthorityNotifiedAt),
		Active: x.Active, Notes: x.Notes, CreatedAt: x.CreatedAt, UpdatedAt: x.UpdatedAt, ETag: ETagOf(x.UpdatedAt)}
	if d.DataCategories == nil {
		d.DataCategories = []string{}
	}
	return d
}

// ---- retention policies

type policyListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []RetentionPolicyDTO `json:"items"`
	}
}

func (h handler) listPolicies(ctx context.Context, _ *struct{}) (*policyListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	ps, err := h.svc.ListRetentionPolicies(ctx, a)
	if err != nil {
		return nil, err
	}
	out := &policyListOutput{CacheControl: "no-store"}
	out.Body.Items = make([]RetentionPolicyDTO, len(ps))
	for i, p := range ps {
		out.Body.Items[i] = toPolicyDTO(p)
	}
	return out, nil
}

type updatePolicyInput struct {
	Code    string `path:"code" pattern:"^[a-z_]{3,60}$" doc:"Policy code (an unknown code is 404)"`
	IfMatch string `header:"If-Match" maxLength:"100" doc:"etag of the list item (428 without, 412 when stale)"`
	Body    struct {
		Period        *string       `json:"period,omitempty" maxLength:"30" doc:"ISO 8601 duration: P3Y, P12M, P30D, PT24H, P0D"`
		Method        *string       `json:"method,omitempty" enum:"hard_delete,anonymize,null_columns,file_delete"`
		Enabled       *bool         `json:"enabled,omitempty"`
		LegalApproved *bool         `json:"legal_approved,omitempty" doc:"The lawyer / the accountant approved the period and method"`
		ApprovedRef   *string       `json:"approved_ref,omitempty" maxLength:"200" doc:"Reference of the approval (document, meeting); required to approve"`
		Reauth        *DsrReauthDTO `json:"reauth,omitempty" doc:"Re-authentication proof (03 §6.2)"`
	}
}

type policyOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         RetentionPolicyDTO
}

func (h handler) updatePolicy(ctx context.Context, in *updatePolicyInput) (*policyOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	p := PolicyPatch{Method: in.Body.Method, Enabled: in.Body.Enabled, LegalApproved: in.Body.LegalApproved, ApprovedRef: in.Body.ApprovedRef}
	if in.Body.Period != nil {
		per, err := ParsePeriod(*in.Body.Period)
		if err != nil {
			return nil, invalid("body.period", "ISO 8601 duration of years, months, days and hours (P3Y, P30D, PT24H)")
		}
		p.Period = &per
	}
	v, err := h.svc.UpdateRetentionPolicy(ctx, a, in.Code, in.IfMatch, p, in.Body.Reauth.proof())
	if err != nil {
		return nil, err
	}
	dto := toPolicyDTO(v)
	return &policyOutput{ETag: dto.ETag, CacheControl: "no-store", Body: dto}, nil
}

// ---- destruction logs

type listLogsInput struct {
	page.Params
	PolicyCode string `query:"policy_code" pattern:"^[a-z_]{3,60}$" doc:"Only this policy"`
	From       string `query:"from" format:"date-time" doc:"Rows written at or after this instant"`
	To         string `query:"to" format:"date-time" doc:"Rows written before this instant"`
}

type logListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items      []DestructionLogDTO `json:"items"`
		NextCursor string              `json:"next_cursor,omitempty"`
	}
}

const sortLogs = "created_at_desc"

func instant(raw, location string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, problem.ErrValidation.WithFields(problem.FieldError{Location: location, Message: "must be an RFC 3339 instant"})
	}
	return &at, nil
}

func (h handler) listLogs(ctx context.Context, in *listLogsInput) (*logListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	f := DestructionFilter{PolicyCode: in.PolicyCode}
	if f.From, err = instant(in.From, "query.from"); err != nil {
		return nil, err
	}
	if f.To, err = instant(in.To, "query.to"); err != nil {
		return nil, err
	}
	fp := page.Fingerprint("adminListDestructionLogs", in.PolicyCode, in.From, in.To)
	after, err := afterOf(in.Cursor, sortLogs, fp)
	if err != nil {
		return nil, err
	}
	p, err := h.svc.ListDestructionLogs(ctx, a, f, after, in.Limit)
	if err != nil {
		return nil, err
	}
	out := &logListOutput{CacheControl: "no-store"}
	out.Body.Items = make([]DestructionLogDTO, len(p.Items))
	for i, l := range p.Items {
		out.Body.Items[i] = toLogDTO(l)
	}
	out.Body.NextCursor = cursorOf(sortLogs, fp, p.Next)
	return out, nil
}

// ---- breaches

type listBreachesInput struct {
	Status string `query:"status" enum:"open,contained,notified,closed"`
}

type breachListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []BreachDTO `json:"items"`
	}
}

func (h handler) listBreaches(ctx context.Context, in *listBreachesInput) (*breachListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var st *BreachStatus
	if in.Status != "" {
		s := BreachStatus(in.Status)
		st = &s
	}
	bs, err := h.svc.ListBreaches(ctx, a, st)
	if err != nil {
		return nil, err
	}
	now := h.clk.Now()
	out := &breachListOutput{CacheControl: "no-store"}
	out.Body.Items = make([]BreachDTO, len(bs))
	for i, b := range bs {
		out.Body.Items[i] = toBreachDTO(b, now)
	}
	return out, nil
}

type createBreachInput struct {
	idempotency.OptionalHeader
	Body struct {
		Title            string    `json:"title" minLength:"1" maxLength:"200" doc:"No person's name"`
		DetectedAt       time.Time `json:"detected_at" doc:"When the breach was detected (the 72 hours start here)"`
		DataCategories   []string  `json:"data_categories" maxItems:"30" doc:"snake_case tokens: identity, contact, health, ..."`
		AffectedEstimate *int      `json:"affected_estimate,omitempty" minimum:"0"`
		Description      string    `json:"description" minLength:"1" maxLength:"10000" doc:"No person's name"`
		OwnerUserID      string    `json:"owner_user_id" format:"uuid" doc:"The staff member handling the breach"`
	}
}

type breachOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         BreachDTO
}

func (h handler) breachOut(b Breach) *breachOutput {
	dto := toBreachDTO(b, h.clk.Now())
	return &breachOutput{ETag: dto.ETag, CacheControl: "no-store", Body: dto}
}

func parseUser(raw, location string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil(), invalid(location, "must be a UUID")
	}
	return id, nil
}

func (h handler) createBreach(ctx context.Context, in *createBreachInput) (*breachOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	owner, err := parseUser(in.Body.OwnerUserID, "body.owner_user_id")
	if err != nil {
		return nil, err
	}
	b, err := h.svc.CreateBreach(ctx, a, BreachInput{Title: in.Body.Title, DetectedAt: in.Body.DetectedAt,
		DataCategories: in.Body.DataCategories, AffectedEstimate: in.Body.AffectedEstimate, Description: in.Body.Description,
		OwnerUserID: owner})
	if err != nil {
		return nil, err
	}
	return h.breachOut(b), nil
}

type updateBreachInput struct {
	BreachID string `path:"breach_id" doc:"Breach ID (an unknown or malformed ID is 404)"`
	IfMatch  string `header:"If-Match" maxLength:"100" doc:"etag of the list item (428 without, 412 when stale)"`
	Body     struct {
		Title                *string    `json:"title,omitempty" minLength:"1" maxLength:"200"`
		Description          *string    `json:"description,omitempty" minLength:"1" maxLength:"10000"`
		DataCategories       *[]string  `json:"data_categories,omitempty" maxItems:"30"`
		AffectedEstimate     *int       `json:"affected_estimate,omitempty" minimum:"0"`
		OwnerUserID          *string    `json:"owner_user_id,omitempty" format:"uuid"`
		DetectedAt           *time.Time `json:"detected_at,omitempty" doc:"Only earlier than recorded"`
		Status               *string    `json:"status,omitempty" enum:"open,contained,notified,closed"`
		ControllerNotifiedAt *time.Time `json:"controller_notified_at,omitempty" doc:"When the data controller was told (written once)"`
		AuthorityNotifiedAt  *time.Time `json:"authority_notified_at,omitempty" doc:"When the Board was notified (written once)"`
	}
}

func (h handler) updateBreach(ctx context.Context, in *updateBreachInput) (*breachOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseIDOrNotFound(in.BreachID)
	if err != nil {
		return nil, err
	}
	p := BreachPatch{Title: in.Body.Title, Description: in.Body.Description, DataCategories: in.Body.DataCategories,
		AffectedEstimate: in.Body.AffectedEstimate, DetectedAt: in.Body.DetectedAt,
		ControllerNotifiedAt: in.Body.ControllerNotifiedAt, AuthorityNotifiedAt: in.Body.AuthorityNotifiedAt}
	if in.Body.Status != nil {
		st := BreachStatus(*in.Body.Status)
		p.Status = &st
	}
	if in.Body.OwnerUserID != nil {
		owner, err := parseUser(*in.Body.OwnerUserID, "body.owner_user_id")
		if err != nil {
			return nil, err
		}
		p.OwnerUserID = &owner
	}
	b, err := h.svc.UpdateBreach(ctx, a, id, in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return h.breachOut(b), nil
}

// ---- sub-processors

type listSubsInput struct {
	Active string `query:"active" enum:"true,false" doc:"Only active (true) or retired (false) sub-processors"`
}

type subListOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         struct {
		Items []SubprocessorDTO `json:"items"`
	}
}

func (h handler) listSubs(ctx context.Context, in *listSubsInput) (*subListOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	var active *bool
	if in.Active != "" {
		v := in.Active == "true"
		active = &v
	}
	xs, err := h.svc.ListSubprocessors(ctx, a, active)
	if err != nil {
		return nil, err
	}
	out := &subListOutput{CacheControl: "no-store"}
	out.Body.Items = make([]SubprocessorDTO, len(xs))
	for i, x := range xs {
		out.Body.Items[i] = toSubDTO(x)
	}
	return out, nil
}

// subBody is the writable part of a sub-processor (02 §16.5).
type subBody struct {
	Name                string   `json:"name" minLength:"1" maxLength:"200"`
	Service             string   `json:"service" minLength:"1" maxLength:"200" doc:"What it does for Koro"`
	DataCategories      []string `json:"data_categories" minItems:"1" maxItems:"30" doc:"snake_case tokens"`
	Region              string   `json:"region" minLength:"1" maxLength:"100" doc:"Hosting region (e.g. eu-central-1)"`
	CountryCode         string   `json:"country_code" pattern:"^[A-Z]{2}$"`
	TransferMechanism   string   `json:"transfer_mechanism" enum:"domestic,standard_contract,adequacy,explicit_consent,undecided"`
	ContractSignedAt    string   `json:"contract_signed_at,omitempty" format:"date"`
	AuthorityNotifiedAt string   `json:"authority_notified_at,omitempty" format:"date" doc:"Standard contract notified to the Board"`
	Notes               string   `json:"notes,omitempty" maxLength:"2000"`
}

type createSubInput struct {
	idempotency.OptionalHeader
	Body subBody
}

type subOutput struct {
	ETag         string `header:"ETag"`
	CacheControl string `header:"Cache-Control"`
	Body         SubprocessorDTO
}

func subOut(x Subprocessor) *subOutput {
	dto := toSubDTO(x)
	return &subOutput{ETag: dto.ETag, CacheControl: "no-store", Body: dto}
}

// optDate parses an optional date field.
func optDate(raw, location string) (*civil.Date, error) {
	if raw == "" {
		return nil, nil
	}
	d, err := civil.Parse(raw)
	if err != nil {
		return nil, invalid(location, "must be a date (YYYY-MM-DD)")
	}
	return &d, nil
}

func (h handler) createSub(ctx context.Context, in *createSubInput) (*subOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	x := SubprocessorInput{Name: b.Name, Service: b.Service, DataCategories: b.DataCategories, Region: b.Region,
		CountryCode: b.CountryCode, TransferMechanism: b.TransferMechanism, Notes: b.Notes}
	if x.ContractSignedAt, err = optDate(b.ContractSignedAt, "body.contract_signed_at"); err != nil {
		return nil, err
	}
	if x.AuthorityNotifiedAt, err = optDate(b.AuthorityNotifiedAt, "body.authority_notified_at"); err != nil {
		return nil, err
	}
	out, err := h.svc.CreateSubprocessor(ctx, a, x)
	if err != nil {
		return nil, err
	}
	return subOut(out), nil
}

type updateSubInput struct {
	SubprocessorID string `path:"subprocessor_id" doc:"Sub-processor ID (an unknown or malformed ID is 404)"`
	IfMatch        string `header:"If-Match" maxLength:"100" doc:"etag of the list item (428 without, 412 when stale)"`
	Body           struct {
		Name                *string   `json:"name,omitempty" minLength:"1" maxLength:"200"`
		Service             *string   `json:"service,omitempty" minLength:"1" maxLength:"200"`
		DataCategories      *[]string `json:"data_categories,omitempty" minItems:"1" maxItems:"30"`
		Region              *string   `json:"region,omitempty" minLength:"1" maxLength:"100"`
		CountryCode         *string   `json:"country_code,omitempty" pattern:"^[A-Z]{2}$"`
		TransferMechanism   *string   `json:"transfer_mechanism,omitempty" enum:"domestic,standard_contract,adequacy,explicit_consent,undecided"`
		ContractSignedAt    *string   `json:"contract_signed_at,omitempty" format:"date"`
		AuthorityNotifiedAt *string   `json:"authority_notified_at,omitempty" format:"date"`
		Active              *bool     `json:"active,omitempty" doc:"false retires the sub-processor (never deleted)"`
		Notes               *string   `json:"notes,omitempty" maxLength:"2000"`
	}
}

func (h handler) updateSub(ctx context.Context, in *updateSubInput) (*subOutput, error) {
	a, err := authz.ActorFrom(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseIDOrNotFound(in.SubprocessorID)
	if err != nil {
		return nil, err
	}
	b := in.Body
	p := SubprocessorPatch{Name: b.Name, Service: b.Service, DataCategories: b.DataCategories, Region: b.Region,
		CountryCode: b.CountryCode, TransferMechanism: b.TransferMechanism, Active: b.Active, Notes: b.Notes}
	if b.ContractSignedAt != nil {
		if p.ContractSignedAt, err = optDate(*b.ContractSignedAt, "body.contract_signed_at"); err != nil {
			return nil, err
		}
	}
	if b.AuthorityNotifiedAt != nil {
		if p.AuthorityNotifiedAt, err = optDate(*b.AuthorityNotifiedAt, "body.authority_notified_at"); err != nil {
			return nil, err
		}
	}
	out, err := h.svc.UpdateSubprocessor(ctx, a, id, in.IfMatch, p)
	if err != nil {
		return nil, err
	}
	return subOut(out), nil
}
