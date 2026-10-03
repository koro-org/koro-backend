package notify

import (
	"bytes"
	"encoding/json"
	"testing"
)

// BuildMIME renders the MIME message the SMTP transport would send for e.
// It lets black-box tests inspect the structure (parts, Content-ID, headers)
// without a server.
func BuildMIME(e Email) ([]byte, error) {
	m, err := newMsg(e)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if _, err := m.WriteTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SNSStringToSign returns the string SNS signs for the message fields e
// (tests sign their fixtures with it).
func SNSStringToSign(t *testing.T, e map[string]any) string {
	t.Helper()
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	var env snsEnvelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatal(err)
	}
	return snsStringToSign(env)
}

// ToDeliveryDTO exposes the admin delivery DTO conversion.
func ToDeliveryDTO(d Delivery) DeliveryDTO { return toDeliveryDTO(d) }
