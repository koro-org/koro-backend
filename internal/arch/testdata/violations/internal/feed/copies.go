package feed

import "errors"

// checkIfMatch is a private copy of httpx.CheckIfMatch.
func checkIfMatch(header, current string) error {
	if header != current {
		return errors.New("precondition failed")
	}
	return nil
}

func reg(api any) {}

// parseID as a method is not a copy.
type handler struct{}

func (handler) parseID(s string) string { return s }

// orgID is not a platform helper name.
func orgID() string { return "" }
