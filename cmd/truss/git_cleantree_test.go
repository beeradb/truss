package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestExecGitCleanTreeAgainstARealRepo is the one direct test of
// execGit.CleanTree against a real `git` binary -- the fake can only record
// that the method was called, never whether the argv it built actually does
// what the comment claims.
//
// ⚠️ IT RUNS THE REAL BINARY AND MUST FAIL, NEVER SKIP, IF GIT IS ABSENT --
// the same AGENTS.md rule every other real-git test in this package cites.
func TestExecGitCleanTreeAgainstARealRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is not on PATH: %v -- this check must fail, not skip, when its own tool is missing", err)
	}

	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")

	tracked := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("committed\n"), 0o644); err != nil {
		t.Fatalf("write tracked.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("*.log\n"), 0o644); err != nil {
		t.Fatalf("write .gitignore: %v", err)
	}
	runGit(t, dir, "add", "-A")
	commit := exec.Command("git", "-C", dir, "commit", "--quiet", "-m", "fixture")
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=truss-test", "GIT_AUTHOR_EMAIL=truss-test",
		"GIT_COMMITTER_NAME=truss-test", "GIT_COMMITTER_EMAIL=truss-test",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// Dirty the tree the way a partial pass would: the tracked file
	// modified, a plain untracked file dropped, an IGNORED file dropped
	// (build cruft matching .gitignore -- only -x reaches this), and a
	// .terraform/ directory, the one thing CleanTree must NOT remove.
	if err := os.WriteFile(tracked, []byte("modified by a pass that did not finish\n"), 0o644); err != nil {
		t.Fatalf("modify tracked.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "junk.txt"), []byte("leftover\n"), 0o644); err != nil {
		t.Fatalf("write junk.txt: %v", err)
	}
	ignored := filepath.Join(dir, "stale.log")
	if err := os.WriteFile(ignored, []byte("ignored build cruft\n"), 0o644); err != nil {
		t.Fatalf("write stale.log: %v", err)
	}
	tfDir := filepath.Join(dir, ".terraform", "plugins")
	if err := os.MkdirAll(tfDir, 0o755); err != nil {
		t.Fatalf("mkdir .terraform/plugins: %v", err)
	}
	marker := filepath.Join(tfDir, "marker")
	if err := os.WriteFile(marker, []byte("provider cache\n"), 0o644); err != nil {
		t.Fatalf("write .terraform marker: %v", err)
	}

	g := execGit{Dir: dir, Stderr: os.Stderr}
	if err := g.CleanTree(context.Background()); err != nil {
		t.Fatalf("CleanTree: %v", err)
	}

	body, err := os.ReadFile(tracked)
	if err != nil {
		t.Fatalf("reading tracked.txt after CleanTree: %v", err)
	}
	if string(body) != "committed\n" {
		t.Errorf("tracked.txt = %q, want the committed content restored by git reset --hard", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "junk.txt")); !os.IsNotExist(err) {
		t.Errorf("junk.txt survived CleanTree, want it removed by git clean -ffdx")
	}
	if _, err := os.Stat(ignored); !os.IsNotExist(err) {
		t.Errorf("stale.log (gitignored) survived CleanTree, want it removed by -x -- without -x an ignored file is exactly what git clean leaves behind")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf(".terraform/plugins/marker did not survive CleanTree (%v), want it preserved by -e .terraform", err)
	}
}
