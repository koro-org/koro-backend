package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"koro/internal/platform/crypto"
	"koro/internal/platform/crypto/openbao"
	"koro/internal/platform/crypto/rewrap"
	"koro/internal/platform/httpx"
	"koro/internal/platform/queue"
)

// cryptoKeys composes platform/crypto (BE-055; 01 §1.3, T-82): the key
// wrapper, the one Envelope every module seals its `*_enc` columns with,
// the OpenBao readiness check and the crypto.* jobs. It is platform, not a
// feature module: wire.go builds it before the modules (c.keys.build) and
// registers its jobs with the platform's (c.keys.tasks).
//
// Key wrapper from the configuration (config.Crypto):
//
//	CRYPTO_OPENBAO_ADDR set   → OpenBao Transit wraps every new value; the
//	                            local KEKs (CRYPTO_KEKS, optional) only open
//	                            values sealed before the switch (crypto.Chain)
//	only CRYPTO_KEKS          → the local KEK (T-82)
//	neither (wiring tests,    → no envelope: envelope() fails, so a module
//	`koro openapi`)             that needs one fails its build
//
// What modules do in their wire_<module>.go build (identity: TOTP secrets,
// events: sensitive answers, parties: IBANs):
//
//	env, err := c.keys.envelope()             // never build a LocalKEK of your own
//	err = c.keys.addRewrap(rewrap.Column{...}) // every *_enc column the module owns
//
// Seams: wrapper (tests preset a crypto.KeyWrapper; nil → configuration).
type cryptoKeys struct {
	wrapper crypto.KeyWrapper

	transit *openbao.Transit // nil without OpenBao
	env     *crypto.Envelope // nil without any key
	columns []rewrap.Column
}

// renewTokenJob renews the periodic OpenBao token (01 §7 addition, BE-055).
const (
	renewTokenJobName = "crypto.renew_token"
	renewTokenCron    = "7 * * * *" //nolint:gosec // G101: a cron spec, not a credential
	renewTokenUnique  = 50 * time.Minute
)

// errNoKey is the answer of envelope() when no key is configured.
var errNoKey = errors.New("crypto: no key-encryption key configured (CRYPTO_OPENBAO_ADDR or CRYPTO_KEKS)")

func (k *cryptoKeys) build(c *container) error {
	w := k.wrapper
	if w == nil {
		cfg := c.infra.platform.Crypto
		var local *crypto.LocalKEK
		if !cfg.KEKs.IsZero() {
			keys, err := crypto.ParseKeyring(cfg.KEKs.Reveal())
			if err != nil {
				return fmt.Errorf("CRYPTO_KEKS: %w", err)
			}
			if local, err = crypto.NewLocalKEK(keys); err != nil {
				return fmt.Errorf("CRYPTO_KEKS: %w", err)
			}
		}
		switch {
		case cfg.OpenBaoEnabled():
			t, err := openbao.New(cfg.OpenBaoConfig(c.infra.clock))
			if err != nil {
				return fmt.Errorf("CRYPTO_OPENBAO_*: %w", err)
			}
			k.transit, w = t, t
			if local != nil {
				if w, err = crypto.NewChain(t, local); err != nil {
					return err
				}
			}
			c.readyChecks = append(c.readyChecks, httpx.Check{Name: "openbao", Run: t.Check})
		case local != nil:
			w = local
		}
	}
	if w == nil {
		return nil
	}
	env, err := crypto.NewEnvelope(w)
	if err != nil {
		return err
	}
	k.env = env
	return nil
}

// envelope returns the shared Envelope (modules call it in build).
func (k *cryptoKeys) envelope() (*crypto.Envelope, error) {
	if k.env == nil {
		return nil, errNoKey
	}
	return k.env, nil
}

// addRewrap declares an envelope-encrypted column for crypto.rewrap. The
// owning module calls it in build.
func (k *cryptoKeys) addRewrap(col rewrap.Column) error {
	if err := col.Validate(); err != nil {
		return err
	}
	for _, c := range k.columns {
		if c.Table == col.Table && c.Column == col.Column {
			return fmt.Errorf("rewrap: column %s.%s declared twice", col.Table, col.Column)
		}
	}
	k.columns = append(k.columns, col)
	return nil
}

// tasks schedules crypto.rewrap (hourly, low) when an envelope and the
// database exist, and crypto.renew_token (hourly, default) with OpenBao.
func (k *cryptoKeys) tasks(c *container) {
	if k.env == nil || c.infra.db == nil {
		return
	}
	log := c.infra.log.With("component", "crypto")
	j, err := rewrap.New(rewrap.Deps{
		ORM: c.infra.db.ORM, Tx: c.infra.db.Tx, Envelope: k.env, Columns: k.columns, Clock: c.infra.clock, Log: log,
	})
	if err != nil { // columns were validated by addRewrap; keep a loud failure anyway
		c.infra.jobs.Schedule(queue.Job{Name: rewrap.JobName, Cron: rewrap.Cron, Queue: queue.Low, Unique: time.Minute,
			Handle: func(context.Context) error { return err }})
		return
	}
	c.infra.jobs.Schedule(j.QueueJob())
	if k.transit != nil {
		c.infra.jobs.Schedule(renewTokenJob(k.transit, log))
	}
}

// tokenRenewer is what crypto.renew_token needs from openbao.Transit.
type tokenRenewer interface {
	RenewToken(ctx context.Context) (bool, error)
}

func renewTokenJob(t tokenRenewer, log *slog.Logger) queue.Job {
	return queue.Job{
		Name: renewTokenJobName, Cron: renewTokenCron, Queue: queue.Default, Unique: renewTokenUnique,
		Handle: func(ctx context.Context) error {
			renewed, err := t.RenewToken(ctx)
			if err != nil {
				log.LogAttrs(ctx, slog.LevelError, "openbao token renewal failed",
					slog.String("error", err.Error()), slog.Bool("alarm", true))
				return err
			}
			log.LogAttrs(ctx, slog.LevelInfo, "openbao token renewal", slog.Bool("renewed", renewed))
			return nil
		},
	}
}
