package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/mail"
	"strings"

	"koro/internal/audit"
	"koro/internal/identity"
	"koro/internal/platform/clock"
	"koro/internal/platform/config"
	"koro/internal/platform/obs"
)

// runSeed runs `koro seed <command>`. bootstrap-admin (BE-014) opens the
// first two platform admins and reference (BE-020) loads the public
// reference data; both may run in every environment. Synthetic demo data
// (BE-033) is dev/staging only (05 §13.7).
func runSeed(ctx context.Context, args []string, environ map[string]string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: missing seed command (bootstrap-admin, reference)", errUsage)
	}
	switch args[0] {
	case "bootstrap-admin":
		return runBootstrapAdmin(ctx, args[1:], environ, stdout)
	case "reference":
		return runSeedReference(ctx, args[1:], environ, stdout)
	default:
		return fmt.Errorf("seed %q: %w (BE-033)", args[0], errNotImplemented)
	}
}

// adminFlag collects repeated -admin "Ad Soyad <e-posta>" values.
type adminFlag []identity.AdminSeed

func (a *adminFlag) String() string { return fmt.Sprintf("%d admins", len(*a)) }

func (a *adminFlag) Set(v string) error {
	s, err := parseAdmin(v)
	if err != nil {
		return err
	}
	*a = append(*a, s)
	return nil
}

// parseAdmin reads "First [Middle] Last <address>"; the last word is the
// last name.
func parseAdmin(v string) (identity.AdminSeed, error) {
	addr, err := mail.ParseAddress(v)
	if err != nil {
		return identity.AdminSeed{}, errors.New(`want "First Last <address>"`)
	}
	names := strings.Fields(addr.Name)
	if len(names) < 2 {
		return identity.AdminSeed{}, errors.New(`want "First Last <address>" with a first and a last name`)
	}
	return identity.AdminSeed{Email: addr.Address, FirstName: strings.Join(names[:len(names)-1], " "), LastName: names[len(names)-1]}, nil
}

// runBootstrapAdmin runs `koro seed bootstrap-admin -admin "…" -admin "…"`
// (02 §5.7, 06 H-17): exactly two platform_admin assignments without a
// second approver, audited, idempotent, CLI only. New accounts have no
// password: the admins set it with the password reset e-mail, then enrol
// TOTP (BE-016).
func runBootstrapAdmin(ctx context.Context, args []string, environ map[string]string, stdout io.Writer) error {
	fs := flag.NewFlagSet("seed bootstrap-admin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var admins adminFlag
	fs.Var(&admins, "admin", `admin as "First Last <address>"; give exactly two`)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 || len(admins) != 2 {
		return fmt.Errorf(`%w: give exactly two -admin "First Last <address>" values`, errUsage)
	}
	cfg, err := config.LoadSeed(environ)
	if err != nil {
		return err
	}
	log := obs.NewLogger(stdout, obs.LoggerOptions{Level: cfg.LogLevel, Format: cfg.LogFormat, Service: "koro-seed", Env: cfg.Env, Version: version})
	clk := clock.Real()
	database, err := openDB(ctx, cfg.Platform.DB, "koro-seed", clk, log)
	if err != nil {
		return err
	}
	defer database.Close()
	store := identity.NewGormStore(database.ORM)
	au := audit.New(audit.Deps{Store: audit.NewGormStore(database.ORM), Tx: database.Tx, Clock: clk, Log: log.With("module", "audit")}).Service
	b, err := identity.NewBootstrapper(identity.BootstrapDeps{Store: store, Tx: database.Tx, Audit: au, Clock: clk, Log: log.With("module", "identity")})
	if err != nil {
		return err
	}
	out, err := b.Run(ctx, admins)
	if err != nil {
		return err
	}
	for i, o := range out {
		state := "already an admin"
		if o.RoleGranted {
			state = "platform_admin granted"
		}
		if o.UserCreated {
			state += "; new account without a password: request a password reset to set it"
		}
		_, _ = fmt.Fprintf(stdout, "admin %d (%s): user %s, %s\n", i+1, obs.MaskEmail(admins[i].Email), o.UserID, state)
	}
	return nil
}
