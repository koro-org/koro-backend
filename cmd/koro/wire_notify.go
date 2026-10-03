package main

import (
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"koro/internal/notify"
	"koro/internal/platform/config"
)

// notifyWiring wires the notify module (L3; 01 §4.5). Owner: BE-005, then
// BE-015. Only tickets of the notify module edit this file (BE-001 R4).
//
// Seams (all nil in production unless noted):
//
//	store      nil → notify.MissingStore (every Send fails with
//	           notify.store_not_wired) until BE-015 installs the GORM store here
//	transport  nil → the SMTP adapter from config (SMTP_*); tests preset a
//	           notify.MemTransport
//	consent    nil → commercial messages are rejected. BE-015 sets it from
//	           c.consent.svc (consent is L2 and is built before notify) via a
//	           small adapter to notify.ConsentChecker
//	templates  nil → the templates embedded in the binary
//	cfg        tests only; nil → the notify section of the loaded configuration
type notifyWiring struct {
	store     notify.Store
	transport notify.EmailTransport
	consent   notify.ConsentChecker
	templates *notify.Templates
	cfg       *config.Notify

	// svc is the sender consumers in higher layers (identity, events, …)
	// adapt to their own port. Set by build.
	svc notify.Service
}

func (w *notifyWiring) build(c *container) error {
	cfg := c.infra.cfg.Notify
	if w.cfg != nil {
		cfg = *w.cfg
	}

	tpl := w.templates
	if tpl == nil {
		var err error
		if tpl, err = notify.EmbeddedTemplates(); err != nil {
			return err // a missing or broken template stops the process at start
		}
	}
	store := w.store
	if store == nil {
		store = notify.MissingStore{}
	}
	transport := w.transport
	if transport == nil {
		var err error
		transport, err = notify.NewSMTPTransport(notify.SMTPConfig{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort, TLS: cfg.SMTPTLS,
			Username: cfg.SMTPUsername, Password: cfg.SMTPPassword.Reveal(), Timeout: cfg.SMTPTimeout,
		})
		if err != nil {
			return fmt.Errorf("smtp transport: %w", err)
		}
	}

	w.svc = notify.New(notify.Deps{
		Store:       store,
		Transport:   transport,
		Templates:   tpl,
		Clock:       c.infra.clock,
		Log:         c.infra.log.With("module", "notify"),
		FromAddress: cfg.SMTPFrom,
		FromName:    cfg.SMTPFromName,
		Consent:     w.consent,
		// Push and InApp stay nil → no-op adapters (BE-055, BE-015).
	}).Service
	return nil
}

// tasks: the outbox subscriptions and jobs of notify arrive with BE-015
// (provider webhook processing) and BE-057 (notify.purge).
func (w *notifyWiring) tasks(c *container) {}

// routes: notify has no endpoints yet (BE-015 adds /me/notifications*).
func (w *notifyWiring) routes(c *container, api huma.API) {}
