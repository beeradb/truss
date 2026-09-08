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
// okRef is a ref checkRef accepts: no leading "-", no whitespace. It is
// deliberately NOT a 40-character hex literal -- scripts/leakscan refuses
// one of those anywhere in the tree and cannot tell a sha fixture from an
// account id, and the guard does not require hex anyway (see checkRef).
const okRef = "commitsha-fixture"

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
	if _, err := g.ChangedFiles(ctx, okRef); err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if _, err := g.TreeRoots(ctx, okRef); err != nil {
		t.Fatalf("TreeRoots: %v", err)
	}
	if err := g.Checkout(ctx, okRef); err != nil {
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
	if err := g.Checkout(context.Background(), okRef); err != nil {
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

// TestGitRefusesARefItShouldNotPass covers §4.4's rule, which was specified
// and never implemented. The ref reaching git comes from the LEDGER -- a
// bucket object -- so anyone able to write applied/HEAD chooses an argument
// to git, and git's option surface is large.
//
// ⚠️ It refuses what is DANGEROUS, not what is unfamiliar. §4.4's literal
// "full hex sha or origin/<branch>" was implemented first and broke
// internal/parity, whose recorded corpus carries the bash suite's own
// synthetic refs. A ref that is merely not a sha cannot do harm; one that
// begins with "-" or carries whitespace can.
func TestGitRefusesARefItShouldNotPass(t *testing.T) {
	bin, envLog := stubGit(t)
	g := execGit{Bin: bin, Dir: t.TempDir()}
	ctx := context.Background()

	for _, ref := range []string{
		"--upload-pack=touch /tmp/pwned", // an option, not a ref
		"-x",
		"main; rm -rf /",
		"origin/main\nx", // a newline smuggled in
		"",
	} {
		if err := g.Checkout(ctx, ref); err == nil {
			t.Errorf("Checkout accepted %q", ref)
		}
		if _, err := g.TreeRoots(ctx, ref); err == nil {
			t.Errorf("TreeRoots accepted %q", ref)
		}
	}

	// The two shapes that ARE legitimate must still pass, or the guard has
	// simply broken the applier.
	if err := g.Checkout(ctx, okRef); err != nil {
		t.Errorf("Checkout refused a full sha: %v", err)
	}
	if _, err := g.Commits(ctx, okRef, "origin/main"); err != nil {
		t.Errorf("Commits refused sha..origin/main: %v", err)
	}
	if _, err := os.ReadFile(envLog); err != nil {
		t.Fatalf("the stub never ran, so the accept cases prove nothing: %v", err)
	}
}

// TestGitGetsAnExplicitEnvironment: the child used to inherit os.Environ(),
// which contradicts the discipline plan.Runner already enforces for tofu --
// and is not merely untidy. GIT_TRACE or GIT_CURL_VERBOSE present in the pod
// would make git print the Authorization header this package is careful to
// keep out of argv and off disk, straight into the pod log. Raised by the
// 2026-09-08 security review.
func TestGitGetsAnExplicitEnvironment(t *testing.T) {
	// A variable that must NOT reach the child. Set on this process only.
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("TRUSS_LEAK_CANARY", "must-not-be-inherited")

	bin, envLog := stubGit(t)
	g := execGit{Bin: bin, Dir: t.TempDir()}.WithToken("fixture")
	if err := g.Checkout(context.Background(), okRef); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	raw, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("reading the stub's env log: %v", err)
	}
	log := string(raw)
	if !strings.Contains(log, "PATH=") {
		t.Fatal("the child got no PATH, so the stub cannot have run normally")
	}
	for _, forbidden := range []string{"GIT_TRACE=", "TRUSS_LEAK_CANARY="} {
		if strings.Contains(log, forbidden) {
			t.Errorf("the child inherited %s from this process; with GIT_TRACE set, git prints the Authorization header", forbidden)
		}
	}
}
