package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubGit writes a fake `git` that appends its own environment to a file
// and exits 0, so a test can see exactly what the child was given.
func stubGit(t *testing.T) (bin, envLog string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "git")
	envLog = filepath.Join(dir, "env.log")
	script := "#!/bin/sh\n" +
		"{ echo \"--- $*\"; env; } >> " + envLog + "\n" +
		"exit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing stub git: %v", err)
	}
	return bin, envLog
}

// TestEveryGitCallCarriesTheInstallationToken covers the 2026-09-08 audit's
// finding: the token reached `clone` and `fetch` and nothing else. The clone
// is --filter=blob:none, so git lazily fetches blobs afterwards -- `diff
// --name-only` does it for rename detection, `checkout` does it to
// materialise a working tree. Unauthenticated, those 401 on a private repo,
// and the error names checkout or diff rather than auth.
func TestEveryGitCallCarriesTheInstallationToken(t *testing.T) {
	// Named authMarker, not token: scripts/leakscan refuses
	// "token = <16+ characters>" anywhere in the tree, and it is right to --
	// it cannot tell a test fixture from a real installation token.
	const authMarker = "MARKER-not-a-real-one"

	bin, envLog := stubGit(t)
	workdir := t.TempDir()
	// Make it look like an existing clone so EnsureClone is a no-op and the
	// later calls are the ones under test.
	if err := os.MkdirAll(filepath.Join(workdir, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	g := execGit{Bin: bin, Dir: workdir}.WithToken(authMarker)

	ctx := context.Background()
	if err := g.Fetch(ctx, "origin", "main"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := g.ChangedFiles(ctx, "deadbeef"); err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if _, err := g.TreeRoots(ctx, "deadbeef"); err != nil {
		t.Fatalf("TreeRoots: %v", err)
	}
	if err := g.Checkout(ctx, "deadbeef"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	raw, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("reading the stub's env log: %v", err)
	}
	log := string(raw)

	// One block per invocation, each starting "--- <args>".
	blocks := strings.Split(log, "--- ")[1:]
	if len(blocks) != 4 {
		t.Fatalf("stub git ran %d times, want 4 (fetch, diff, ls-tree, checkout):\n%s", len(blocks), log)
	}
	for _, b := range blocks {
		args := strings.SplitN(b, "\n", 2)[0]
		if !strings.Contains(b, "GIT_CONFIG_KEY_0=http.extraheader") {
			t.Errorf("git %s ran without the auth header: a blobless clone will fetch unauthenticated here", args)
		}
	}

	// The credential travels in the environment only -- never argv, never a
	// URL, and never `git config`-ed into the clone, which would put it on
	// disk (the leak the port removed by taking it out of remote.origin.url).
	for _, b := range blocks {
		args := strings.SplitN(b, "\n", 2)[0]
		if strings.Contains(args, authMarker) {
			t.Errorf("the token appears in argv: git %s", args)
		}
	}
	if _, err := os.Stat(filepath.Join(workdir, ".git", "config")); err == nil {
		cfg, _ := os.ReadFile(filepath.Join(workdir, ".git", "config"))
		if strings.Contains(string(cfg), authMarker) {
			t.Error("the token was written into the clone's git config")
		}
	}
}

// TestAGitDriverWithNoTokenSendsNoAuthHeader keeps the test above honest:
// it must be the token that puts the header there, not something that is
// always present.
func TestAGitDriverWithNoTokenSendsNoAuthHeader(t *testing.T) {
	bin, envLog := stubGit(t)
	workdir := t.TempDir()

	g := execGit{Bin: bin, Dir: workdir}
	if err := g.Checkout(context.Background(), "deadbeef"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	raw, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("reading the stub's env log: %v", err)
	}
	if strings.Contains(string(raw), "GIT_CONFIG_KEY_0") {
		t.Error("an untokened driver still set the auth header, so the other test proves nothing")
	}
}

// TestNoHTTPClientIsUnbounded: http.DefaultClient has NO timeout, so a
// server that accepts a connection and then never answers hangs the caller
// forever. The caller here is a CronJob firing every five minutes, so a hung
// pass is a growing pile of pods rather than one stuck process. internal/forge
// was the only package that had bounded itself; the 2026-09-08 security review
// raised the rest as an inconsistency.
//
// Enforced as a source scan rather than by inspecting a constructed client,
// because the defect is a package DEFAULTING to http.DefaultClient -- which is
// invisible from outside once the struct is built.
func TestNoHTTPClientIsUnbounded(t *testing.T) {
	roots := []string{"..", "../../internal"}
	scanned := 0
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for i, line := range strings.Split(string(src), "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") {
					continue // the comments explaining this rule name it
				}
				if strings.Contains(line, "http.DefaultClient") {
					t.Errorf("%s:%d uses http.DefaultClient, which has no timeout:\n\t%s",
						path, i+1, trimmed)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files, so this check could not have failed")
	}
}
