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

// TestExecGitTreeAnsibleUnitsIgnoresAPlaysOwnSubdirectories is the
// integration half of the 2026-09-12 wedge, and the reason it is written
// against a real tree rather than against KindOf alone: the bug needed BOTH
// halves to bite. TreeAnsibleUnits lists with `ls-tree -d -r`, which is
// recursive on purpose -- without -r the only line back is "ansible/plays"
// itself -- and classification then accepted every directory it returned.
//
// So a play carrying Ansible's own standard layout (group_vars/, host_vars/)
// arrived as THREE plays, two of them with no site.yml and no host declaring
// them. The target gate refused every play in the pass, every five minutes,
// until the engine was fixed. A unit test on KindOf would not have caught the
// pairing; this one fails on the real listing.
//
// ⚠️ IT RUNS THE REAL BINARY AND MUST FAIL, NEVER SKIP, IF GIT IS ABSENT --
// the same rule TestExecGitTreeTofuUnitsAgainstARealRepo states: a check
// nobody has watched fail is a claim.
func TestExecGitTreeAnsibleUnitsIgnoresAPlaysOwnSubdirectories(t *testing.T) {
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
	// One play, laid out the way Ansible documents: variables beside the
	// playbook, in directories of their own.
	write("ansible/plays/dev-workstation/site.yml", "---\n# a play\n")
	write("ansible/plays/dev-workstation/group_vars/all.yml", "---\nlogin_user: somebody\n")
	write("ansible/plays/dev-workstation/host_vars/a-host.yml", "---\nproject_dir: /tmp\n")
	// A second play, so "exactly one" cannot pass by there being only one
	// directory in the tree at all.
	write("ansible/plays/node-exporter/site.yml", "---\n# another play\n")
	write("ansible/plays/node-exporter/group_vars/all.yml", "---\nport: 9100\n")

	runGit(t, dir, "add", "-A")
	commit := exec.Command("git", "-C", dir, "commit", "--quiet", "-m", "fixture")
	commit.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=truss-test", "GIT_AUTHOR_EMAIL=truss-test",
		"GIT_COMMITTER_NAME=truss-test", "GIT_COMMITTER_EMAIL=truss-test",
	)
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	sha := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))

	g := execGit{Bin: "git", Dir: dir, Stderr: io.Discard}
	got, err := g.TreeAnsibleUnits(context.Background(), sha)
	if err != nil {
		t.Fatalf("TreeAnsibleUnits: %v", err)
	}
	sort.Strings(got)
	want := []string{"ansible/plays/dev-workstation", "ansible/plays/node-exporter"}
	if len(got) != len(want) {
		t.Fatalf("TreeAnsibleUnits = %v, want exactly %v -- a play's own group_vars/ and host_vars/ are not plays", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("TreeAnsibleUnits = %v, want exactly %v", got, want)
		}
	}
}
