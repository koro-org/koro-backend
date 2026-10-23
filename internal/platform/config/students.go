package config

import (
	"fmt"
	"time"
)

// Students configures the students module (internal/students). Only tickets
// of the students module edit this file and the "# --- students ---" block of
// .env.example (BE-001 R7).
type Students struct {
	// GrantSuspensionWindow is how long a level 3 grant suspended by a lapsed
	// student verification stays open (BE-088; user decision Q-03, T-331: 90
	// days). Within it the student may resume the grant once verified again
	// (Q-04, T-332); after it students.expire_suspended_grants revokes it.
	GrantSuspensionWindow time.Duration `env:"STUDENTS_GRANT_SUSPENSION_WINDOW" envDefault:"2160h"`
}

func (s *Students) validate(Common) error {
	if s.GrantSuspensionWindow < 24*time.Hour || s.GrantSuspensionWindow > 365*24*time.Hour {
		return fmt.Errorf("STUDENTS_GRANT_SUSPENSION_WINDOW must be between 24h and 8760h (got %s)", s.GrantSuspensionWindow)
	}
	return nil
}
