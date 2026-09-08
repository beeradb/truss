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
// cfBaseURL overrides the Cloudflare API host, read from
// $CLOUDFLARE_API_BASE_URL. Empty leaves the real one in place.
//
// ⚠️ IT EXISTS FOR THE SAME REASON $GITHUB_API_BASE_URL DOES, AND IT WAS
// MISSING. secrets.CloudflareToken already declares a BaseURL field whose
// doc says it "overrides the Cloudflare API host for tests" -- and nothing
// ever set it, so the probe reached the real api.cloudflare.com from every
// run including a test one. That made the one credential whose lapse takes
// the applier down the one credential no whole-pass test could drive
// (internal/parity, added 2026-09-08, is what could not be written without
// this), and it meant any such test would egress from CI. Optional, never
// part of config.Config's required names, and never a route a credential
// can travel -- the same shape loadForgeConfig's baseURL has.
func runExpirySweep(ctx context.Context, cfg config.Config, dir secrets.Dir, vcfg secrets.KVConfig, cfBaseURL string, now func() time.Time) ([]secrets.Expiring, error) {
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
			itemCFTokenMint: secrets.CloudflareToken{BaseURL: cfBaseURL, Token: mintToken},
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
	findings, err := runExpirySweep(ctx, cfg, dir, vcfg, getenv("CLOUDFLARE_API_BASE_URL"), time.Now)
	if err != nil {
		// ⚠️ PRINT WHAT THE SWEEP DID LEARN, THEN FAIL. Sweep.Run evaluates
		// the live PROBES before it walks any store and returns those
		// findings alongside its error -- so discarding them here threw away
		// the only real expiry data the system currently has. Measured
		// 2026-09-08: `truss expiry` printed nothing but "platform lists 6
		// item(s) but not one records an expiry", because one empty store
		// suppressed the Cloudflare probe's answer about cf-token-mint.
		//
		// This does NOT soften §4.7. The exit code stays 1 and the error
		// still goes to stderr, so nothing reads this as a clean bill of
		// health; the findings go to stdout where a partial answer is
		// strictly more useful than none. A sweep that cannot say whether
		// anything is expiring must say so -- it need not also forget what
		// it already found out.
		fmt.Fprintf(stderr, "expiry: %v\n", err)
		if len(findings) > 0 {
			fmt.Fprintln(stderr, "expiry: what the probes did answer, before the failure above:")
			if encErr := json.NewEncoder(stdout).Encode(findings); encErr != nil {
				fmt.Fprintf(stderr, "expiry: encoding partial findings: %v\n", encErr)
			}
		}
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
