package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExecGitTreeFSAgainstARealRepo is the one direct test of execGit.TreeFS
// against a real `git` binary, the same shape TestExecGitTreeRenderUnitsAgainstARealRepo
// is for TreeRenderUnits: every other test in this package drives TreeFS
// through fakeGit, so this is the only place a regression in the real
// ls-tree/show plumbing -- as opposed to the fakeGit seam standing in for it
// -- would be caught.
//
// ⚠️ IT RUNS THE REAL BINARY AND MUST FAIL, NEVER SKIP, IF GIT IS ABSENT. The
// same AGENTS.md rule TestExecGitTreeRenderUnitsAgainstARealRepo cites: a
// t.Skip on a missing tool reports "passing" on a machine where the check
// never ran, which is silence dressed as success.
//
// The point of TreeFS is reading a commit's tree WITHOUT checking it out --
// see execGit.TreeFS's own doc -- so this test never runs `git checkout` at
// all; it reads straight from the commit object the way inventory.Load's
// caller does.
func TestExecGitTreeFSAgainstARealRepo(t *testing.T) {
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
	const hostBody = `{"schema":"truss.host/v1","name":"a","kind":"vm","role":"unmanaged"}`
	const kustBody = "kind: Kustomization\n"
	write("inventory/hosts/a.json", hostBody)
	write("deliveries/c/u/kustomization.yaml", kustBody)

	runGit(t, dir, "add", "-A")
	// A fixed identity via env, never a repo-local `git config` and never
	// whatever the machine happens to have configured -- AGENTS.md: a test
	// must not depend on the machine running it.
	commit := exec.Command("git", "-C", dir, "commit", "--quiet", "-m", "fixture")
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=truss-test", "GIT_AUTHOR_EMAIL=truss-test",
		// ⚠️ NO @ IN THE ADDRESS -- scripts/leakscan refuses anything shaped
		// like an email anywhere in this repository, and it cannot tell a
		// fixture's address from a real one.
		"GIT_COMMITTER_NAME=truss-test", "GIT_COMMITTER_EMAIL=truss-test",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	sha := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	// ⚠️ THE WORKING TREE IS EMPTIED AFTER THE COMMIT, ON PURPOSE. The
	// fixture files had to exist on disk to be committed in the first place,
	// so their mere presence proves nothing about TreeFS. Removing them here
	// and reading them back THROUGH TreeFS is what proves it reads from the
	// commit object rather than the worktree -- and reading through a real
	// checkout would simply fail once these are gone.
	if err := os.RemoveAll(filepath.Join(dir, "inventory")); err != nil {
		t.Fatalf("removing inventory/ from the worktree: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "deliveries")); err != nil {
		t.Fatalf("removing deliveries/ from the worktree: %v", err)
	}

	g := execGit{Bin: "git", Dir: dir, Stderr: io.Discard}
	treeFS, err := g.TreeFS(context.Background(), sha)
	if err != nil {
		t.Fatalf("TreeFS: %v", err)
	}

	gotHost, err := fs.ReadFile(treeFS, "inventory/hosts/a.json")
	if err != nil {
		t.Fatalf("reading inventory/hosts/a.json through TreeFS: %v", err)
	}
	if string(gotHost) != hostBody {
		t.Errorf("inventory/hosts/a.json = %q, want %q", gotHost, hostBody)
	}

	gotKust, err := fs.ReadFile(treeFS, "deliveries/c/u/kustomization.yaml")
	if err != nil {
		t.Fatalf("reading deliveries/c/u/kustomization.yaml through TreeFS: %v", err)
	}
	if string(gotKust) != kustBody {
		t.Errorf("deliveries/c/u/kustomization.yaml = %q, want %q", gotKust, kustBody)
	}

	// Still nothing checked out, after two reads through TreeFS: the files
	// were read successfully above despite being absent from the worktree,
	// and they must still be absent now -- TreeFS materialised nothing.
	for _, p := range []string{"inventory", "deliveries"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Fatalf("%s exists in the worktree after reading through TreeFS -- it checked something out", p)
		}
	}

	// An absent path is refused in the fs.ErrNotExist shape callers of an
	// fs.FS are entitled to assume (fs.Stat, fs.ReadDir and inventory.Load's
	// own fs.Stat("inventory") check all depend on this).
	if _, err := fs.ReadFile(treeFS, "inventory/hosts/nope.json"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("reading an absent path: err = %v, want an fs.ErrNotExist-shaped error", err)
	}
	if _, err := fs.Stat(treeFS, "does/not/exist"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat of an absent path: err = %v, want an fs.ErrNotExist-shaped error", err)
	}
}
