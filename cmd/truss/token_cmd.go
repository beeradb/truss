package main

import (
	"context"
	"fmt"
	"io"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/forge"
	"github.com/beeradb/truss/internal/secrets"
)

// cmdToken replaces gh-app-token: mint a fresh GitHub App installation
// token and print it to stdout with no trailing newline, matching the
// bash's own `printf '%s'` (§4.9). Exit 0 on success; on any failure the
// message is on stderr and nothing resembling a token is ever printed --
// forge.Client already redacts its own errors (TestNoTokenAppearsInAnyError).
func cmdToken(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss token")
		return 2
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	client, err := buildForgeClient(cfg, getenv)
	if err != nil {
		fmt.Fprintf(stderr, "token: %v\n", err)
		return 1
	}

	tok, _, err := client.InstallationToken(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "token: %v\n", err)
		return 1
	}
	fmt.Fprint(stdout, tok)
	return 0
}

// buildForgeClient reads the GitHub App's credentials from secrets.Dir and
// constructs a forge.Client, or refuses. getenv supplies the one optional
// override loadForgeConfig accepts ($GITHUB_API_BASE_URL); production
// callers pass the process's real getenv and get the real host.
func buildForgeClient(cfg config.Config, getenv func(string) string) (*forge.Client, error) {
	dir := secrets.Dir{Root: cfg.SecretsDir}
	fcfg, err := loadForgeConfig(dir, cfg.Repo, getenv("GITHUB_API_BASE_URL"))
	if err != nil {
		return nil, err
	}
	return forge.New(fcfg)
}
