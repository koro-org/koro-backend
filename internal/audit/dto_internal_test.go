package audit

import (
	"testing"
	"uuid"
)

// TestAuditEventDTO_CarriesTheRecordID (BE-127): every audit list item has
// the stable record ID, the key of client lists and deep links.
func TestAuditEventDTO_CarriesTheRecordID(t *testing.T) {
	t.Parallel()
	id := uuid.NewV7()
	if got := toAuditEventDTO(View{ID: id, Action: "orgs.org.update"}); got.EventID != id {
		t.Fatalf("event_id = %v, want %v", got.EventID, id)
	}
	l := toAuditEventList(Page[View]{Items: []View{{ID: id}}}, "")
	if len(l.Items) != 1 || l.Items[0].EventID != id {
		t.Fatalf("list = %+v", l.Items)
	}
}
