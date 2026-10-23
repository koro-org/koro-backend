package messaging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"
	"uuid"

	"koro/internal/authz"
	"koro/internal/identity"
	"koro/internal/notify"
	"koro/internal/orgs"
	"koro/internal/platform/domainevent"
	"koro/internal/platform/queue"
)

// Worker names of messaging (01 §6.2, §7).
const (
	ExpireJobName          = "messaging.expire_requests"
	NotifyReceivedTaskName = "messaging.notify_request_received"
	NotifyClosedTaskName   = "messaging.notify_request_closed"
	// ExpireBatch is how many requests one expiry transaction closes.
	ExpireBatch = 200
)

// Notice templates (internal/notify/templates/messaging): in_app, push and
// email (T-336; BE-089), preference contact_requests on each channel. The
// in-app and push texts are fixed: the data carries the request's ID, the
// deep link the request, the in-app source org the company. The e-mail
// adds the company's name (to the student) and the link. Never the subject,
// the message or a reason.
const (
	tmplReceived = "messaging.contact_request_received" // the student: a company asked
	tmplAccepted = "messaging.contact_request_accepted" // the sender: accepted (level 3 is open)
	tmplDeclined = "messaging.contact_request_declined" // the sender: declined (no reason)
	tmplExpired  = "messaging.contact_request_expired"  // the sender: 14 days without an answer
)

// expire closes a pending request past its 14 days in the caller's
// transaction (the job, a create that meets it, a block): the status, the
// company's notice (O) and the audit record of the system. The unit is not
// given back (01 §7). ok=false when it is no longer pending.
func (s *service) expire(ctx context.Context, r ContactRequest) (bool, error) {
	next, ok, err := s.d.Store.Transition(ctx, r.ID, StatusPending, StatusExpired, nil)
	if err != nil || !ok {
		return false, err
	}
	if err := s.emit(ctx, domainevent.ContactRequestClosed{
		ContactRequestID: next.ID, OrgID: next.OrgID, Outcome: string(StatusExpired),
	}, next.ID); err != nil {
		return false, err
	}
	org := next.OrgID
	ev := requestEvent(authz.MessagingContactRequestExpire, next, &org, map[string]any{"unit_released": false})
	return true, s.d.Audit.Log(ctx, ev.By(authz.SystemActor(ExpireJobName)))
}

// expireJob is messaging.expire_requests (01 §7: every hour at :40,
// default queue).
func (s *service) expireJob() queue.Job {
	return queue.Job{Name: ExpireJobName, Cron: "40 * * * *", Queue: queue.Default, Unique: 50 * time.Minute,
		Handle: s.ExpireDue}
}

// ExpireDue closes every pending request past its 14 days, in batches of
// ExpireBatch (one transaction each; rows another worker holds are
// skipped). It returns how many it closed through the log.
func (s *service) ExpireDue(ctx context.Context) error {
	if err := authz.Can(ctx, authz.SystemActor(ExpireJobName), authz.MessagingContactRequestExpire, authz.Resource{}); err != nil {
		return err
	}
	total := 0
	for {
		var n, seen int
		err := s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
			rows, err := s.d.Store.DueForExpiry(ctx, s.now(), ExpireBatch)
			if err != nil {
				return err
			}
			seen = len(rows)
			for _, r := range rows {
				ok, err := s.expire(ctx, r)
				if err != nil {
					return err
				}
				if ok {
					n++
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		total += n
		if seen < ExpireBatch {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if total > 0 {
		s.d.Log.LogAttrs(ctx, slog.LevelInfo, "contact requests expired", slog.Int("count", total))
	}
	return nil
}

// handleCreated tells the student that a company asked (in-app; the company
// is the notice's source org). A request no longer pending (withdrawn or
// answered before the task ran) or gone (erased) is skipped.
func (s *service) handleCreated(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ContactRequestCreated](e)
	if err != nil {
		return queue.Permanent(err)
	}
	r, err := s.d.Store.GetForStudent(ctx, p.StudentUserID, p.ContactRequestID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	case !r.Open(s.now()):
		return nil
	}
	org := r.OrgID
	company, err := s.companyName(ctx, r.OrgID)
	if err != nil {
		return err
	}
	return s.notify(ctx, tmplReceived, r, []uuid.UUID{r.StudentUserID}, "/me/contact-requests/"+r.ID.String(), &org,
		map[string]any{"company_name": company})
}

// companyName is the company's name for the student's e-mail: the org's
// public name (the sender of the request, never the student's data); a
// company gone meanwhile reads as an empty name (the template's generic
// wording).
func (s *service) companyName(ctx context.Context, orgID uuid.UUID) (string, error) {
	o, err := s.d.Orgs.Get(ctx, orgID)
	switch {
	case errors.Is(err, orgs.ErrNotFound):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("messaging: notice company: %w", err)
	}
	return o.Name, nil
}

// handleClosed tells the company the outcome: the sender while still an
// active member of it, otherwise its owner and admins. No reason exists to
// tell.
func (s *service) handleClosed(ctx context.Context, e domainevent.Envelope) error {
	p, err := domainevent.Decode[domainevent.ContactRequestClosed](e)
	if err != nil {
		return queue.Permanent(err)
	}
	var tmpl string
	switch Status(p.Outcome) {
	case StatusAccepted:
		tmpl = tmplAccepted
	case StatusDeclined:
		tmpl = tmplDeclined
	case StatusExpired:
		tmpl = tmplExpired
	default:
		return queue.Permanent(fmt.Errorf("messaging: closed notice: unknown outcome %q", p.Outcome))
	}
	r, err := s.d.Store.Get(ctx, p.OrgID, p.ContactRequestID)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // erased with the student's account
	case err != nil:
		return err
	}
	to, err := s.companyRecipients(ctx, r)
	if err != nil {
		return err
	}
	return s.notify(ctx, tmpl, r, to, "/orgs/"+r.OrgID.String()+"/contact-requests/"+r.ID.String(), nil, nil)
}

// companyRecipients is the sender while an active member of the company,
// otherwise the company's owner and admins.
func (s *service) companyRecipients(ctx context.Context, r ContactRequest) ([]uuid.UUID, error) {
	ids, err := s.d.Orgs.ActiveMemberIDs(ctx, r.OrgID)
	if err != nil {
		return nil, fmt.Errorf("messaging: org members: %w", err)
	}
	if slices.Contains(ids, r.SenderUserID) {
		return []uuid.UUID{r.SenderUserID}, nil
	}
	var out []uuid.UUID
	for _, id := range ids {
		ok, err := s.d.Orgs.HasRole(ctx, r.OrgID, id, authz.MemberCompanyOwner, authz.MemberCompanyAdmin)
		if err != nil {
			return nil, fmt.Errorf("messaging: org member role: %w", err)
		}
		if ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// noticeChannels are the channels of every contact request notice (T-336):
// notify drops the ones a recipient switched off (contact_requests).
var noticeChannels = []notify.Channel{notify.ChannelInApp, notify.ChannelPush, notify.ChannelEmail}

// notify sends tmpl to every recipient on each channel: in-app (the source
// org, the deep link), push (the deep link) and e-mail (the address and
// language from identity; mail plus the link to the request). The dedupe
// key per recipient and channel makes a repeated task a no-op where it
// already went (the in-app key is BE-056's). A recipient without an
// address (erased meanwhile) gets no e-mail; a permanent refusal is
// logged, a transient one retries the task.
func (s *service) notify(ctx context.Context, tmpl string, r ContactRequest, to []uuid.UUID, link string, source *uuid.UUID,
	mail map[string]any,
) error {
	if len(to) == 0 {
		return nil
	}
	if s.d.Sender == nil {
		return errors.New("messaging: notify is not wired")
	}
	var errs []error
	for _, uid := range to {
		u := uid
		key := tmpl + ":" + r.ID.String() + ":" + u.String()
		for _, ch := range noticeChannels {
			m := notify.Message{
				UserID: &u, Channel: ch, Category: notify.CategoryTransactional, Template: tmpl, LinkPath: link,
				DedupeKey: key, Data: map[string]any{"contact_request_id": r.ID.String()},
			}
			switch ch {
			case notify.ChannelInApp:
				m.SourceOrgID = source
			case notify.ChannelPush:
				m.DedupeKey = key + ":push"
			case notify.ChannelEmail:
				c, ok, err := s.contact(ctx, u)
				if err != nil {
					errs = append(errs, err)
				}
				if !ok {
					continue
				}
				m.ToEmail, m.Locale, m.LinkPath, m.DedupeKey = c.Email, c.Locale, "", key+":email"
				m.Data = map[string]any{"url": s.d.LinkBaseURL + link}
				for k, v := range mail {
					m.Data[k] = v
				}
			}
			if _, err := s.d.Sender.Send(ctx, m); err != nil {
				if !notify.IsPermanent(err) {
					errs = append(errs, err)
					continue
				}
				s.d.Log.LogAttrs(ctx, slog.LevelError, "contact request notice not deliverable", slog.String("template", tmpl),
					slog.String("channel", string(ch)), slog.String("contact_request_id", r.ID.String()),
					slog.String("user_id", u.String()), slog.String("err", err.Error()))
			}
		}
	}
	return errors.Join(errs...)
}

// contact is the address of a notice's e-mail; none without the Users
// port or for an unknown or erased user.
func (s *service) contact(ctx context.Context, id uuid.UUID) (identity.UserContact, bool, error) {
	if s.d.Users == nil {
		return identity.UserContact{}, false, nil
	}
	c, err := s.d.Users.Contact(ctx, id)
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return identity.UserContact{}, false, nil
	case err != nil:
		return identity.UserContact{}, false, fmt.Errorf("messaging: notice contact: %w", err)
	}
	return c, c.Email != "", nil
}
