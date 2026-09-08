package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/secrets"
)

// vaultRequiredEnv names the four variables NewKV needs that
// internal/config deliberately does not carry -- a Vault mount, role and
// JWT path are not credentials in Config's sense, but §4.1 never mentions
// them, and Config's own doc is explicit that roots, vaults and the like
// are refused rather than made configurable through it. cmd/truss reads
// them itself, with the same fail-closed shape config.Load uses: every
// problem is reported, none is defaulted or guessed
// (TestNoMountRoleOrItemNameIsHardcoded in internal/secrets is the sibling
// rule this respects one level up).
var vaultRequiredEnv = []string{"VAULT_ADDR", "VAULT_ROLE", "VAULT_JWT_PATH", "VAULT_MOUNT"}

// loadVaultConfig reads the Vault KV mount's connection details from the
// environment, refusing (with every problem, not just the first) if any is
// unset or empty -- matching config.Load's own contract.
func loadVaultConfig(getenv func(string) string) (secrets.KVConfig, []string) {
	values := make(map[string]string, len(vaultRequiredEnv))
	var problems []string
	for _, name := range vaultRequiredEnv {
		v := getenv(name)
		if v == "" {
			problems = append(problems, fmt.Sprintf("refusing to start: $%s is unset", name))
		}
		values[name] = v
	}
	if len(problems) > 0 {
		return secrets.KVConfig{}, problems
	}
	return secrets.KVConfig{
		Addr:    values["VAULT_ADDR"],
		Mount:   values["VAULT_MOUNT"],
		Role:    values["VAULT_ROLE"],
		JWTPath: values["VAULT_JWT_PATH"],
	}, nil
}

// runExpirySweep builds the Vault store and the Cloudflare probe and runs
// one sweep, shared by `truss expiry` and the apply pass's own end-of-run
// nag (§4.7, §2 item 8's "the pass always writes a heartbeat" applies to
// the apply pass; this function is just the sweep itself, deliberately
// unopinionated about what happens with its result).
//
// ⚠️ Only the "platform" Vault mount is swept. Decision 4's own gap
// analysis (docs/port-plan.md §4.7, "A SECOND GAP, LARGER THAN DECISION 4
// STATES") records that the bash also swept a 1Password "platform" vault
// and the whole "recipes-runtime" vault, neither of which has a Vault
// mount yet -- Sweep.Stores is a slice specifically so this is a
// configuration change once a second mount exists, not a code change. That
// gap is real today and is not fixed here.
func runExpirySweep(ctx context.Context, cfg config.Config, dir secrets.Dir, vcfg secrets.KVConfig, now func() time.Time) ([]secrets.Expiring, error) {
	mintToken, err := loadCFMintToken(dir)
	if err != nil {
		return nil, err
	}
	kv, err := secrets.NewKV(vcfg)
	if err != nil {
		return nil, err
	}
	sweep := secrets.Sweep{
		Stores: []secrets.Store{kv},
		Probes: map[string]secrets.Probe{
			itemCFTokenMint: secrets.CloudflareToken{Token: mintToken},
		},
		WarnDays: cfg.ExpiryWarnDays,
		Now:      now,
	}
	return sweep.Run(ctx)
}

// cmdExpiry replaces check_credential_lifetimes as a standalone subcommand
// (§4.9): run one sweep and print its findings as JSON. Exit 1 (message on
// stderr) if the sweep itself failed -- never a synonym for "nothing is
// expiring" (§4.7) -- exit 0 otherwise, findings or not.
func cmdExpiry(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss expiry")
		return 2
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}
	vcfg, vproblems := loadVaultConfig(getenv)
	if len(vproblems) > 0 {
		for _, p := range vproblems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	dir := secrets.Dir{Root: cfg.SecretsDir}
	findings, err := runExpirySweep(ctx, cfg, dir, vcfg, time.Now)
	if err != nil {
		fmt.Fprintf(stderr, "expiry: %v\n", err)
		return 1
	}

	if findings == nil {
		findings = []secrets.Expiring{}
	}
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(findings); err != nil {
		fmt.Fprintf(stderr, "expiry: encoding findings: %v\n", err)
		return 1
	}
	return 0
}
