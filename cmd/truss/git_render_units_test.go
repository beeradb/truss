package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestExecGitTreeRenderUnitsAgainstARealRepo is the one direct test of
// execGit.TreeRenderUnits against an actual `git` binary. Before this, the
// method had zero coverage of its own: every cmd/truss test drives it
// through fakeGit, and no internal/parity scenario carries a deliveries/
// directory, so replacing the whole body with `return nil, nil` left the
// suite green -- parity included. Contrast TreeRoots, whose ls-tree twin is
// exercised in git_token_test.go and TestGitRefusesARefItShouldNotPass, and
// whose same mutation fails three parity scenarios.
//
// ⚠️ IT RUNS THE REAL BINARY AND MUST FAIL, NEVER SKIP, IF GIT IS ABSENT.
// AGENTS.md's rule is that a check nobody has watched fail is a claim, and a
// t.Skip on a missing tool would let this test report "passing" on a machine
// where it never ran at all -- silence dressed as success is exactly the
// failure mode this whole repository refuses.
//
// The tree is built to catch three specific ways this could go wrong:
//
//   - THE DEPTH TRAP: `ls-tree -d -r` is required because a baseline sits one
//     level under its prefix and a delivery sits two, so a plain `-d` (no
//     `-r`) can reach one depth or the other but never both. baselines/prod
//     and deliveries/beta/web together are what would expose a regression to
//     plain -d: either the baseline or the delivery would silently drop out.
//   - A WRONG PATHSPEC: deliveries/README.md is a FILE directly under
//     deliveries/, not a directory, so `-d` alone must already exclude it;
//     it is here to catch a listing that stopped filtering on "is a
//     directory".
//   - repo.KindOf DRIFT: clusters/beta/main.tf is a real unit shape
//     (KindTofu), included so a KindOf that started accepting it as
//     KindRender -- rather than refusing it, kept out of the render
//     listing -- would be caught here rather than only in internal/repo's
//     own tests.
func TestExecGitTreeRenderUnitsAgainstARealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not on PATH: %v -- this check must fail, not skip, when its own tool is missing", err)
	}

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")

	write := func(rel, body string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("baselines/prod/kustomization.yaml", "kind: Kustomization\n")
	write("deliveries/beta/web/kustomization.yaml", "kind: Kustomization\n")
	write("deliveries/README.md", "not a unit, a file directly under deliveries/\n") // decoy: file, not a directory
	write("clusters/beta/main.tf", "# a tofu root, not a render unit\n")             // decoy: a real unit of a different kind

	runGit(t, dir, "add", "-A")
	// Env rather than a repo-local `git config`, and a fixed identity rather
	// than whatever happens to be configured: AGENTS.md's own rule is that a
	// test must not depend on the machine running it, and this one passing
	// only because some box has user.email set is exactly the shape of bug
	// that rule exists to prevent.
	commit := exec.Command("git", "-C", dir, "commit", "--quiet", "-m", "fixture")
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=truss-test", "GIT_AUTHOR_EMAIL=truss-test@truss-test.invalid",
		"GIT_COMMITTER_NAME=truss-test", "GIT_COMMITTER_EMAIL=truss-test@truss-test.invalid",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	sha := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	g := execGit{Bin: "git", Dir: dir, Stderr: io.Discard}
	got, err := g.TreeRenderUnits(context.Background(), sha)
	if err != nil {
		t.Fatalf("TreeRenderUnits: %v", err)
	}
	sort.Strings(got)
	want := []string{"baselines/prod", "deliveries/beta/web"}
	if len(got) != len(want) {
		t.Fatalf("TreeRenderUnits = %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TreeRenderUnits = %v, want exactly %v", got, want)
		}
	}
}

// runGit runs git in dir and returns its stdout, failing the test on any
// error -- there is no case in this file where a git command is expected to
// fail, so any error here is a fixture problem, not something a caller
// inspects.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}
