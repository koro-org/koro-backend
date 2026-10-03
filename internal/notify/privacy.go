package notify

import (
	"context"
	"time"
	"uuid"

	"koro/internal/platform/privacy"
)

// privacyProvider is the KVKK provider of notify (01 §3.3). Export lists the
// user's deliveries (IDs, channel, template, status; never content),
// preferences, push tokens (never the token) and in-app notifications.
// Erase deletes all four. The suppression list is not linked to a user
// (blind index only) and stays: it honours a complaint or a dead mailbox
// (02 §6.2, retention email_suppression).
type privacyProvider struct{ s *service }

var _ privacy.Provider = privacyProvider{}

// Name implements privacy.Provider.
func (privacyProvider) Name() string { return "notify" }

// exportPage bounds one export read; the tables are bounded by retention
// (deliveries 12 months, in-app 6 months).
const exportPage = 1000

// Export implements privacy.Provider.
func (p privacyProvider) Export(ctx context.Context, userID uuid.UUID) (privacy.ExportSection, error) {
	st := p.s.d.Store
	out := privacy.ExportSection{Module: "notify", Items: []map[string]any{}}

	var after *Position
	for {
		rows, err := st.ListOfUser(ctx, userID, after, exportPage)
		if err != nil {
			return privacy.ExportSection{}, err
		}
		for _, d := range rows {
			item := map[string]any{"kind": "delivery", "delivery_id": d.ID.String(), "channel": string(d.Channel),
				"category": string(d.Category), "template": d.Template, "status": string(d.Status),
				"created_at": d.CreatedAt.UTC().Format(time.RFC3339)}
			if d.SentAt != nil {
				item["sent_at"] = d.SentAt.UTC().Format(time.RFC3339)
			}
			out.Items = append(out.Items, item)
		}
		if len(rows) < exportPage {
			break
		}
		last := rows[len(rows)-1]
		after = &Position{At: last.CreatedAt, ID: last.ID}
	}

	prefs, err := st.Preferences(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, pr := range prefs {
		out.Items = append(out.Items, map[string]any{"kind": "preference", "category": string(pr.Category),
			"channel": string(pr.Channel), "enabled": pr.Enabled})
	}

	tokens, err := st.PushTokensOf(ctx, userID)
	if err != nil {
		return privacy.ExportSection{}, err
	}
	for _, t := range tokens {
		item := map[string]any{"kind": "push_token", "push_token_id": t.ID.String(), "platform": string(t.Platform),
			"created_at": t.CreatedAt.UTC().Format(time.RFC3339), "last_seen_at": t.LastSeenAt.UTC().Format(time.RFC3339)}
		if t.AppVersion != "" {
			item["app_version"] = t.AppVersion
		}
		out.Items = append(out.Items, item)
	}

	after = nil
	for {
		rows, err := st.ListInApp(ctx, userID, false, after, exportPage)
		if err != nil {
			return privacy.ExportSection{}, err
		}
		for _, n := range rows {
			item := map[string]any{"kind": "in_app_notification", "notification_id": n.ID.String(), "type": n.Kind,
				"title": n.Title, "body": n.Body, "created_at": n.CreatedAt.UTC().Format(time.RFC3339)}
			if n.ReadAt != nil {
				item["read_at"] = n.ReadAt.UTC().Format(time.RFC3339)
			}
			out.Items = append(out.Items, item)
		}
		if len(rows) < exportPage {
			break
		}
		last := rows[len(rows)-1]
		after = &Position{At: last.CreatedAt, ID: last.ID}
	}
	return out, nil
}

// Erase implements privacy.Provider. It is idempotent: a second run deletes
// nothing.
func (p privacyProvider) Erase(ctx context.Context, userID uuid.UUID) (privacy.EraseResult, error) {
	res := privacy.EraseResult{Module: "notify"}
	err := p.s.d.Tx.WithTx(ctx, func(ctx context.Context) error {
		st := p.s.d.Store
		for _, del := range []func(context.Context, uuid.UUID) (int, error){
			st.DeleteInAppOf, st.DeletePushTokensOf, st.DeletePreferencesOf, st.DeleteDeliveriesOf,
		} {
			n, err := del(ctx, userID)
			if err != nil {
				return err
			}
			res.Deleted += n
		}
		return nil
	})
	if err != nil {
		return privacy.EraseResult{}, err
	}
	return res, nil
}
