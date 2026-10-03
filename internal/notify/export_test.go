package notify

import "bytes"

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
