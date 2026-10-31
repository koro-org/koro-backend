package identity

// Store is the persistence boundary of identity (05 §2.2). No identity
// table is org-owned (02 §5: "Org kapsamı: yok"): every method is cross-org
// and addresses rows by user, token hash or ID. The GORM store
// (store_gorm.go) and the in-memory store (store_mem.go) pass the same
// contract test (store_contract_test.go).
//
// Every method works in the transaction carried by ctx, if any. Raw tokens
// never reach the store: sessions and e-mail tokens are found by the
// SHA-256 of the token.
//
// Store is split by aggregate into embedded interfaces (05 §2.2, BE-148);
// a service file reaches the store only through the narrow view of the
// aggregate it works on (the accessors below; the bootstrap through its
// own), the retention sweeper through the views it holds (retention.go),
// the KVKK provider (privacy.go), which covers every table of a data
// subject, through the whole Store.
type Store interface {
	userStore          // users, the account (store_users.go)
	sessionStore       // sessions (store_sessions.go)
	emailTokenStore    // email_tokens (store_email_tokens.go)
	roleStore          // platform_role_assignments (store_roles.go)
	mfaStore           // mfa_totp and mfa_recovery_codes (store_mfa.go)
	accountActionStore // privileged_account_actions, four eyes (store_account_actions.go)
	subjectStore       // what only the KVKK provider does to a data subject's rows (store_subject.go)

	EduStore       // edu_verifications (store_edu.go)
	RetentionStore // the purges of the retention sweeper (retention.go)
	StatsStore     // the account counts of the platform panel (stats.go, BE-163)
}

// users is the account view of the store.
func (s *service) users() userStore { return s.d.Store }

// sessions is the sessions view of the store.
func (s *service) sessions() sessionStore { return s.d.Store }

// emailTokens is the email_tokens view of the store.
func (s *service) emailTokens() emailTokenStore { return s.d.Store }

// roles is the platform role assignments view of the store.
func (s *service) roles() roleStore { return s.d.Store }

// mfa is the second factor view of the store (TOTP, recovery codes).
func (s *service) mfa() mfaStore { return s.d.Store }

// accountActions is the four-eyes account action view of the store.
func (s *service) accountActions() accountActionStore { return s.d.Store }

// edu is the student verification view of the store.
func (s *service) edu() EduStore { return s.d.Store }

// stats is the account count view of the store (BE-163).
func (s *service) stats() StatsStore { return s.d.Store }

// users is the account view of the bootstrap's store.
func (b *Bootstrapper) users() userStore { return b.d.Store }

// roles is the platform role assignments view of the bootstrap's store.
func (b *Bootstrapper) roles() roleStore { return b.d.Store }

// sessions is the sessions view of the bootstrap's store.
func (b *Bootstrapper) sessions() sessionStore { return b.d.Store }
