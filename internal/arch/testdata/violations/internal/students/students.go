package students

import (
	"koro/internal/authz/authztest"
	"koro/internal/identity"
	"koro/internal/parties"
)

var _, _, _ = authztest.ID, identity.Users(nil), parties.Payees(nil)
