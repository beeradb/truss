package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beeradb/truss/internal/render"
)

// fakeKustomizeCmd writes an executable standing in for the real kustomize
// binary, in the shape internal/render/render_test.go's own fakeKustomize
// uses: a #!/bin/sh script run with "$@", so a test controls stdout, stderr
// and the exit status. Every invocation appends its environment to envFile,
// which is how TestRenderDigestDoesNotLeakTheParentEnvironment inspects what
// the child actually got rather than what the caller meant to give it.
func fakeKustomizeCmd(t *testing.T, body string) (bin, envFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "kustomize")
	envFile = filepath.Join(dir, "env")
	script := "#!/bin/sh\n" +
		"env >> " + envFile + "\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake kustomize: %v", err)
	}
	return bin, envFile
}

// TestRenderDigestHappyPathPrintsHexWithNoTrailingNewline pins the wire
// contract CI relies on: 64 lowercase hex characters, no trailing newline --
// the same contract plan-digest's own stdout keeps, so both are consumed by
// the same CI shell.
func TestRenderDigestHappyPathPrintsHexWithNoTrailingNewline(t *testing.T) {
	bin, _ := fakeKustomizeCmd(t, `printf 'kind: Service\n'`)
	t.Setenv("KUSTOMIZE_BIN", bin)

	var stdout, stderr bytes.Buffer
	code := run([]string{"render-digest", t.TempDir()}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	got := stdout.String()
	if len(got) != 64 {
		t.Fatalf("stdout length = %d, want 64: %q", len(got), got)
	}
	for _, c := range got {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("stdout = %q, want only lowercase hex characters", got)
		}
	}
	if strings.HasSuffix(got, "\n") {
		t.Fatalf("stdout ends with a newline: %q", got)
	}
	if want := render.Digest([]byte("kind: Service\n")); got != want {
		t.Fatalf("stdout = %q, want render.Digest of the fake's stdout: %q", got, want)
	}
}

// TestRenderDigestRefusesANonDeterministicRender is the check CI runs so a
// unit with a random input is caught the day it is added rather than the day
// the applier refuses a digest nobody can reproduce (render.BuildStable's
// own doc comment).
func TestRenderDigestRefusesANonDeterministicRender(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	bin, _ := fakeKustomizeCmd(t, `printf 'x' >> `+counter+`; wc -c < `+counter)
	t.Setenv("KUSTOMIZE_BIN", bin)

	var stdout, stderr bytes.Buffer
	code := run([]string{"render-digest", t.TempDir()}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "non-deterministic") {
		t.Errorf("stderr = %q, want it to name non-determinism", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failure", stdout.String())
	}
}

// TestRenderDigestOnKustomizeFailurePassesStderrThrough checks kustomize's
// own transcript reaches the command's stderr rather than being swallowed
// or folded into the error message (render.Build never carries it in the
// error, precisely so an operator can still read it on Stderr).
func TestRenderDigestOnKustomizeFailurePassesStderrThrough(t *testing.T) {
	bin, _ := fakeKustomizeCmd(t, `echo "kustomize-transcript-line" >&2; exit 3`)
	t.Setenv("KUSTOMIZE_BIN", bin)

	var stdout, stderr bytes.Buffer
	code := run([]string{"render-digest", t.TempDir()}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "kustomize-transcript-line") {
		t.Errorf("stderr = %q, want kustomize's own stderr to reach the command's stderr", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty on failure", stdout.String())
	}
}

// TestRenderDigestWrongArity covers both directions: no directory and too
// many arguments are both a usage error, not a render attempt.
func TestRenderDigestWrongArity(t *testing.T) {
	cases := [][]string{
		{"render-digest"},
		{"render-digest", "a", "b"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
	}
}

// TestRenderDigestDoesNotLeakTheParentEnvironment pins that Runner.Env is
// built from exactly PATH and HOME, never the process's full environment --
// the same rule apply_cmd.go's buildBaseEnv keeps and internal/render's own
// TestBuildDoesNotInheritTheEnvironment pins from the other side.
func TestRenderDigestDoesNotLeakTheParentEnvironment(t *testing.T) {
	t.Setenv("TRUSS_RENDER_DIGEST_CANARY", "must-not-be-inherited")
	bin, envFile := fakeKustomizeCmd(t, `printf 'kind: Service\n'`)
	t.Setenv("KUSTOMIZE_BIN", bin)

	var stdout, stderr bytes.Buffer
	code := run([]string{"render-digest", t.TempDir()}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading env: %v", err)
	}
	// ⚠️ THE BUFFER IS NEVER PRINTED -- this fails exactly when the
	// environment was inherited, which is when dumping it is most dangerous.
	// On a public CI runner that log is readable by anybody.
	if strings.Contains(string(env), "must-not-be-inherited") {
		t.Error("the canary reached the child: render-digest inherited the parent environment")
	}
}
