package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/secrets"
)

// cmdLedger dispatches `ledger get <key>` and `ledger put <key>`, exposing
// the ledger's read and write paths as commands (§4.9). Both need the full
// config -- internal/config has no partial-load path, and the bucket name
// alone is not enough to build a ledger.Store (§4.2 needs the endpoint and
// credentials too, which live under secrets.Dir).
func cmdLedger(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || (args[0] != "get" && args[0] != "put") {
		fmt.Fprintln(stderr, "usage: truss ledger get <key> | truss ledger put <key>")
		return 2
	}
	verb, key := args[0], args[1]

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	store, err := buildLedgerStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "ledger %s: %v\n", verb, err)
		return 1
	}

	switch verb {
	case "get":
		body, err := store.Get(ctx, key)
		if err != nil {
			if errors.Is(err, ledger.ErrNotFound) {
				return 2
			}
			fmt.Fprintf(stderr, "ledger get: %v\n", err)
			return 1
		}
		stdout.Write(body)
		return 0
	case "put":
		body, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "ledger put: reading stdin: %v\n", err)
			return 1
		}
		if err := store.Put(ctx, key, body); err != nil {
			fmt.Fprintf(stderr, "ledger put: %v\n", err)
			return 1
		}
		return 0
	}
	return 2
}

// buildLedgerStore reads the ledger's endpoint and credentials from
// secrets.Dir and constructs a Store, or refuses.
func buildLedgerStore(cfg config.Config) (*ledger.Store, error) {
	dir := secrets.Dir{Root: cfg.SecretsDir}
	lcfg, err := loadLedgerConfig(dir, cfg.LedgerBucket)
	if err != nil {
		return nil, err
	}
	return ledger.New(lcfg)
}

// layoutFor builds the ledger.Layout config.Config already validated names.
func layoutFor(cfg config.Config) ledger.Layout {
	return ledger.Layout{
		AppliedPrefix:    cfg.LedgerAppliedPrefix,
		FailedPrefix:     cfg.LedgerFailedPrefix,
		HeadKey:          cfg.LedgerHeadKey,
		HeartbeatKey:     cfg.HeartbeatKey,
		PlanDigestPrefix: cfg.PlanDigestPrefix,
	}
}
