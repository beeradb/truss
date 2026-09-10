package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKustomize writes an executable standing in for the real binary. body
// is shell run with "$@" available, so a test can control stdout, stderr and
// the exit status. Every invocation appends its argv to argvFile and its
// environment to envFile, which is how the two tests below inspect what the
// child was actually given rather than what the caller meant to give it.
func fakeKustomize(t *testing.T, body string) (bin, argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "kustomize")
	argvFile = filepath.Join(dir, "argv")
	envFile = filepath.Join(dir, "env")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + argvFile + "\n" +
		"env >> " + envFile + "\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake kustomize: %v", err)
	}
	return bin, argvFile, envFile
}

func TestBuildReturnsExactlyStdout(t *testing.T) {
	bin, _, _ := fakeKustomize(t, `printf 'kind: Service\n'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	got, err := r.Build(context.Background(), "deliveries/beta/web")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if string(got) != "kind: Service\n" {
		t.Errorf("Build = %q, want the renderer's stdout verbatim", got)
	}
}

// TestBuildNeverPassesEnableHelm is the no-Helm rule, checked where it is
// actually enforced. kustomize refuses a helmCharts field on its own when the
// flag is absent (measured on v5.7.1: exit 1, "must specify --enable-helm"),
// so the only way truss can weaken that is by passing the flag.
//
// ⚠️ IT ASSERTS THE EXACT ARGV, NOT MERELY THE ABSENCE OF --enable-helm.
// Both sides of the digest gate -- CI's render-digest and the applier's
// render pass -- call this same Build, so a Build that dropped dir from its
// argv would run `kustomize build` with no path on both sides: the process's
// working directory, rendered twice, agreeing byte for byte and proving
// nothing about the tree the commit actually touched. Checking "contains
// build" and "lacks enable-helm" both stayed true under that mutation; only
// pinning the full argv catches it.
func TestBuildNeverPassesEnableHelm(t *testing.T) {
	bin, argvFile, _ := fakeKustomize(t, `printf 'x\n'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	if _, err := r.Build(context.Background(), "deliveries/beta/web"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading argv: %v", err)
	}
	if got, want := string(argv), "build\ndeliveries/beta/web\n"; got != want {
		t.Fatalf("argv = %q, want exactly %q", got, want)
	}
}

// TestBuildDoesNotInheritTheEnvironment pins that a nil Env means an empty
// environment and not the parent's. exec.Cmd's own default is the opposite,
// so this is the difference between the renderer seeing nothing and the
// renderer seeing every variable the applier holds.
func TestBuildDoesNotInheritTheEnvironment(t *testing.T) {
	t.Setenv("TRUSS_RENDER_CANARY", "must-not-be-inherited")
	bin, _, envFile := fakeKustomize(t, `printf 'x\n'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	if _, err := r.Build(context.Background(), "d"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading env: %v", err)
	}
	// ⚠️ THE BUFFER IS NEVER PRINTED. This assertion fails exactly when the
	// environment WAS inherited, which is precisely when dumping it is most
	// dangerous: on a public CI runner that log is readable by anybody, and
	// the parent environment is where every credential lives. Report the
	// canary's presence, never the haystack. scripts/leakscan cannot catch
	// this class -- it scans the source, and the secret arrives at runtime.
	if strings.Contains(string(env), "must-not-be-inherited") {
		t.Error("the canary reached the child: Build inherited the parent environment")
	}
}

// TestBuildRefusesAnEmptyRender covers the case where the digest of nothing
// would otherwise agree with the digest of every other nothing.
func TestBuildRefusesAnEmptyRender(t *testing.T) {
	bin, _, _ := fakeKustomize(t, `true`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	if _, err := r.Build(context.Background(), "d"); err == nil {
		t.Fatal("Build accepted an empty render; a gate whose sides can agree on emptiness passes on garbage")
	}
}

func TestBuildRefusesWithoutABinaryOrADirectory(t *testing.T) {
	if _, err := (Runner{Stderr: io.Discard}).Build(context.Background(), "d"); err == nil {
		t.Error("Build accepted an empty Bin; the renderer version is part of the digest contract")
	}
	bin, _, _ := fakeKustomize(t, `printf 'x\n'`)
	if _, err := (Runner{Bin: bin, Stderr: io.Discard}).Build(context.Background(), ""); err == nil {
		t.Error("Build accepted an empty directory")
	}
}

// TestBuildRefusesANilStderr is Bin's own refusal, applied to Stderr: a nil
// io.Writer does not panic exec.Cmd, it is silently read as "discard", so
// without this check a render that failed for a reason nobody kept would
// fail quietly rather than loudly -- exactly the transcript this package
// exists to preserve (Stderr's own doc comment).
func TestBuildRefusesANilStderr(t *testing.T) {
	bin, _, _ := fakeKustomize(t, `printf 'x\n'`)
	if _, err := (Runner{Bin: bin}).Build(context.Background(), "d"); err == nil {
		t.Error("Build accepted a nil Stderr; a failed render's transcript would be silently discarded")
	}
}

// TestBuildErrorDoesNotCarryTheTranscript mirrors plan.wrapExecError: an
// error string here reaches the ledger and a chat message, and a renderer's
// output can carry paths and URLs out of a tree this process does not
// control. The transcript belongs on Stderr, where an operator reads it.
func TestBuildErrorDoesNotCarryTheTranscript(t *testing.T) {
	bin, _, _ := fakeKustomize(t, `echo "sensitive-transcript-line" >&2; exit 3`)
	var stderr strings.Builder
	r := Runner{Bin: bin, Stderr: &stderr}
	_, err := r.Build(context.Background(), "d")
	if err == nil {
		t.Fatal("Build did not fail on a non-zero exit")
	}
	if strings.Contains(err.Error(), "sensitive-transcript-line") {
		t.Errorf("error = %q, want the transcript kept out of it", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error = %q, want it to name the exit status", err)
	}
	if !strings.Contains(stderr.String(), "sensitive-transcript-line") {
		t.Errorf("stderr = %q, want the transcript delivered there", stderr.String())
	}
}

// TestBuildStableRefusesANonDeterministicRender is the check CI runs so that
// a chart or generator with a random input is caught the day it lands rather
// than the day the applier refuses a digest nobody can reproduce.
func TestBuildStableRefusesANonDeterministicRender(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	bin, _, _ := fakeKustomize(t, `printf 'x' >> `+counter+`; wc -c < `+counter)
	r := Runner{Bin: bin, Stderr: io.Discard}
	_, err := r.BuildStable(context.Background(), "d")
	if err == nil {
		t.Fatal("BuildStable accepted a render that differs between runs")
	}
	if !strings.Contains(err.Error(), "non-deterministic") {
		t.Errorf("error = %q, want it to name the cause", err)
	}
}

func TestBuildStableAcceptsAStableRender(t *testing.T) {
	bin, _, _ := fakeKustomize(t, `printf 'kind: Service\n'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	got, err := r.BuildStable(context.Background(), "d")
	if err != nil {
		t.Fatalf("BuildStable: %v", err)
	}
	if string(got) != "kind: Service\n" {
		t.Errorf("BuildStable = %q", got)
	}
}

func TestDigestIsSHA256Hex(t *testing.T) {
	// The expectation is computed rather than written down, and not only
	// because scripts/leakscan refuses a 64-character hex literal: what is
	// worth pinning here is the ALGORITHM, since changing it would invalidate
	// every digest already recorded in the bucket. Comparing against the
	// standard library's sha256 catches that; a copied constant would too,
	// but would also have to be re-derived by hand every time anyone wanted
	// to check it was right.
	want := hex.EncodeToString(func() []byte { sum := sha256.Sum256(nil); return sum[:] }())
	if got := Digest(nil); got != want {
		t.Errorf("Digest(nil) = %q, want the sha256 of no bytes", got)
	}
	if got := Digest([]byte("a")); len(got) != 64 {
		t.Errorf("Digest = %q, want 64 hex characters", got)
	}
	if Digest([]byte("a")) == Digest([]byte("b")) {
		t.Error("two different renders hashed the same")
	}
}

// TestBuildCannotReachTheNetwork pins the proxy settings every render runs
// under. The property they defend was measured rather than assumed:
// `kustomize build` resolves a remote `resources:` URL over the network by
// default, with no flag to disable it, and a proxy pointed at a closed port
// is what stops it -- exit 1, zero bytes, an error naming the URL.
//
// Without this the two sides of the digest could read different inputs, or
// the same unreviewed ones, and the gate would certify manifests nobody saw.
func TestBuildCannotReachTheNetwork(t *testing.T) {
	bin, _, envFile := fakeKustomize(t, `printf 'x\n'`)
	r := Runner{Bin: bin, Env: []string{"PATH=/usr/bin", "NO_PROXY=github.com"}, Stderr: io.Discard}
	if _, err := r.Build(context.Background(), "d"); err != nil {
		t.Fatalf("Build: %v", err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading env: %v", err)
	}
	got := string(env)
	for _, want := range []string{
		"HTTPS_PROXY=" + blackholeProxy,
		"https_proxy=" + blackholeProxy,
		"HTTP_PROXY=" + blackholeProxy,
		"ALL_PROXY=" + blackholeProxy,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("child environment lacks %q", want)
		}
	}
	// A caller's own NO_PROXY must not survive: it is the one variable that
	// would wave a host straight past the blackhole.
	if strings.Contains(got, "NO_PROXY=github.com") {
		t.Error("a caller's NO_PROXY survived; it must be emptied, not merged")
	}
}
