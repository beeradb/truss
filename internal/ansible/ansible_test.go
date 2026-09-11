package ansible

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAnsiblePlaybook writes an executable standing in for the real binary,
// the same shape render_test.go's fakeKustomize uses: body is shell run
// with "$@" available, and every invocation appends its argv to argvFile and
// its environment to envFile, so a test can inspect what the child was
// actually given rather than what the caller meant to give it.
func fakeAnsiblePlaybook(t *testing.T, body string) (bin, argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "ansible-playbook")
	argvFile = filepath.Join(dir, "argv")
	envFile = filepath.Join(dir, "env")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + argvFile + "\n" +
		"env >> " + envFile + "\n" +
		body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing fake ansible-playbook: %v", err)
	}
	return bin, argvFile, envFile
}

// newPlay makes a REAL play directory, because run() now stats
// <playDir>/site.yml before exec. A test that wants the argv must have one
// on disk; a test that passes a path with no site.yml is testing the
// refusal, not the argv, and says so.
func newPlay(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dev-beta")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("making play dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "site.yml"), []byte("- hosts: all\n"), 0o600); err != nil {
		t.Fatalf("writing site.yml: %v", err)
	}
	return dir
}

// jsonCallback is a minimal well-formed JSON callback document naming two
// hosts, .invalid so scripts/leakscan never has a real hostname to refuse.
const jsonCallback = `{"stats":{"host1.invalid":{"changed":2},"host2.invalid":{"changed":0}}}`

func TestApplyBuildsExactArgvWithTheLimit(t *testing.T) {
	bin, argvFile, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	dir := newPlay(t)
	r := Runner{Bin: bin, Stderr: io.Discard}
	_, err := r.Apply(context.Background(), dir, []string{"host1.invalid", "host2.invalid"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading argv: %v", err)
	}
	want := filepath.Join(dir, "site.yml") + "\n--limit\nhost1.invalid,host2.invalid\n"
	if got := string(argv); got != want {
		t.Fatalf("argv = %q, want exactly %q", got, want)
	}
}

// TestCheckAddsTheCheckFlagAndNothingElse pins that Check mode differs from
// Apply by exactly one flag, appended after the limit -- not a second
// binary, not a different argument order that could hide a divergence.
func TestCheckAddsTheCheckFlagAndNothingElse(t *testing.T) {
	bin, argvFile, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	dir := newPlay(t)
	r := Runner{Bin: bin, Stderr: io.Discard}
	_, err := r.Check(context.Background(), dir, []string{"host1.invalid"})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading argv: %v", err)
	}
	want := filepath.Join(dir, "site.yml") + "\n--limit\nhost1.invalid\n--check\n"
	if got := string(argv); got != want {
		t.Fatalf("argv = %q, want exactly %q", got, want)
	}
}

// TestRunRefusesAnEmptyHostsSliceBeforeExec pins the ⚠️ rule: a play with no
// --limit runs against every host in the inventory, so an empty hosts slice
// must never reach exec at all. The binary records its own invocation into
// argvFile, so "argvFile was never created" is the proof exec never ran.
func TestRunRefusesAnEmptyHostsSliceBeforeExec(t *testing.T) {
	bin, argvFile, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	dir := newPlay(t)
	r := Runner{Bin: bin, Stderr: io.Discard}
	if _, err := r.Apply(context.Background(), dir, nil); err == nil {
		t.Fatal("Apply accepted an empty hosts slice")
	}
	if _, err := r.Check(context.Background(), dir, []string{}); err == nil {
		t.Fatal("Check accepted an empty hosts slice")
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Fatal("the binary ran despite an empty hosts slice: argvFile exists")
	}
}

func TestRunRefusesWithoutABinaryOrAPlayDir(t *testing.T) {
	if _, err := (Runner{Stderr: io.Discard}).Apply(context.Background(), newPlay(t), []string{"host1.invalid"}); err == nil {
		t.Error("Apply accepted an empty Bin")
	}
	bin, _, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	if _, err := (Runner{Bin: bin, Stderr: io.Discard}).Apply(context.Background(), "", []string{"host1.invalid"}); err == nil {
		t.Error("Apply accepted an empty play directory")
	}
}

func TestRunRefusesANilStderr(t *testing.T) {
	bin, _, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	if _, err := (Runner{Bin: bin}).Apply(context.Background(), newPlay(t), []string{"host1.invalid"}); err == nil {
		t.Error("Apply accepted a nil Stderr")
	}
}

// TestRunDoesNotInheritTheEnvironment pins that a nil Env means an empty
// environment and not the parent's -- the same rule and the same reason
// render_test.go's TestBuildDoesNotInheritTheEnvironment states: exec.Cmd's
// own default is the opposite.
//
// ⚠️ THE BUFFER IS NEVER PRINTED. Report the canary's presence, never the
// haystack -- a failure message that dumps an environment is a credential
// leak in a public CI log, and scripts/leakscan cannot catch this class
// because the secret arrives at runtime, not in source.
func TestRunDoesNotInheritTheEnvironment(t *testing.T) {
	t.Setenv("TRUSS_ANSIBLE_CANARY", "must-not-be-inherited")
	bin, _, envFile := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	if _, err := r.Apply(context.Background(), newPlay(t), []string{"host1.invalid"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading env: %v", err)
	}
	if strings.Contains(string(env), "must-not-be-inherited") {
		t.Error("the canary reached the child: Apply inherited the parent environment")
	}
}

// TestRunAlwaysSetsTheJSONCallback pins that the caller cannot weaken the
// JSON-callback requirement by setting the variable itself -- later entries
// win in exec, so Runner's own append must be the one that lands.
func TestRunAlwaysSetsTheJSONCallback(t *testing.T) {
	bin, _, envFile := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	r := Runner{Bin: bin, Env: []string{"ANSIBLE_STDOUT_CALLBACK=yaml"}, Stderr: io.Discard}
	if _, err := r.Apply(context.Background(), newPlay(t), []string{"host1.invalid"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading env: %v", err)
	}
	if !strings.Contains(string(env), "ANSIBLE_STDOUT_CALLBACK=json") {
		t.Error("the JSON callback was not the effective setting")
	}
}

// TestRunErrorDoesNotCarryTheTranscript mirrors
// render_test.go's TestBuildErrorDoesNotCarryTheTranscript and
// plan.wrapExecError: a play's transcript can carry host detail, and an
// error string here reaches the ledger and a chat message.
func TestRunErrorDoesNotCarryTheTranscript(t *testing.T) {
	bin, _, _ := fakeAnsiblePlaybook(t, `echo "sensitive-host-detail" >&2; exit 3`)
	var stderr strings.Builder
	r := Runner{Bin: bin, Stderr: &stderr}
	_, err := r.Apply(context.Background(), newPlay(t), []string{"host1.invalid"})
	if err == nil {
		t.Fatal("Apply did not fail on a non-zero exit")
	}
	if strings.Contains(err.Error(), "sensitive-host-detail") {
		t.Errorf("error = %q, want the transcript kept out of it", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("error = %q, want it to name the exit status", err)
	}
	if !strings.Contains(stderr.String(), "sensitive-host-detail") {
		t.Errorf("stderr = %q, want the transcript delivered there", stderr.String())
	}
}

// TestApplyParsesPerHostChangedCounts is the load-bearing parse test: the
// JSON callback's stats object, read field by field, never a scrape of the
// human-readable PLAY RECAP.
func TestApplyParsesPerHostChangedCounts(t *testing.T) {
	bin, _, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	result, err := r.Apply(context.Background(), newPlay(t), []string{"host1.invalid", "host2.invalid"})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := result.ChangedByHost["host1.invalid"], 2; got != want {
		t.Errorf("host1.invalid changed = %d, want %d", got, want)
	}
	if got, want := result.ChangedByHost["host2.invalid"], 0; got != want {
		t.Errorf("host2.invalid changed = %d, want %d", got, want)
	}
	if string(result.Raw) != jsonCallback {
		t.Errorf("Raw = %q, want the callback output verbatim", result.Raw)
	}
}

// TestApplyRefusesMalformedOutput: malformed output must be an error, never
// an empty Result -- an empty map and a parse failure both look like
// "nothing changed", and a caller that cannot tell them apart cannot record
// either fact honestly.
func TestApplyRefusesMalformedOutput(t *testing.T) {
	bin, _, _ := fakeAnsiblePlaybook(t, `printf 'not json at all'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	result, err := r.Apply(context.Background(), newPlay(t), []string{"host1.invalid"})
	if err == nil {
		t.Fatal("Apply accepted malformed output")
	}
	if result.ChangedByHost != nil || result.Raw != nil {
		t.Errorf("Apply returned a non-zero Result alongside an error: %+v", result)
	}
}

// TestApplyRefusesOutputWithNoStatsObject: well-formed JSON that is not the
// callback's shape (no "stats" key) must also be refused, not read as zero
// hosts changed.
func TestApplyRefusesOutputWithNoStatsObject(t *testing.T) {
	bin, _, _ := fakeAnsiblePlaybook(t, `printf '{"plays":[]}'`)
	r := Runner{Bin: bin, Stderr: io.Discard}
	if _, err := r.Apply(context.Background(), newPlay(t), []string{"host1.invalid"}); err == nil {
		t.Fatal("Apply accepted output with no stats object")
	}
}

// TestRunHandsAnsibleTheEntrypointAndNotTheDirectory is the regression. A
// real pass on 2026-09-10 failed with ansible's "does not appear to be a
// file" because argv[0] was the play DIRECTORY.
func TestRunHandsAnsibleTheEntrypointAndNotTheDirectory(t *testing.T) {
	bin, argvFile, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	dir := newPlay(t)
	if _, err := (Runner{Bin: bin, Stderr: io.Discard}).Apply(context.Background(), dir, []string{"host1.invalid"}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading argv: %v", err)
	}
	first := strings.SplitN(string(argv), "\n", 2)[0]
	if first == dir {
		t.Fatalf("argv[0] is the play directory %q -- ansible needs a file", first)
	}
	if want := filepath.Join(dir, "site.yml"); first != want {
		t.Fatalf("argv[0] = %q, want %q", first, want)
	}
}

// TestRunRefusesAPlayWithNoEntrypointBeforeExec pins that the refusal
// happens in this package and not inside ansible: argvFile is written by the
// fake binary on every invocation, so its absence proves exec never ran.
func TestRunRefusesAPlayWithNoEntrypointBeforeExec(t *testing.T) {
	bin, argvFile, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	empty := t.TempDir()
	_, err := (Runner{Bin: bin, Stderr: io.Discard}).Apply(context.Background(), empty, []string{"host1.invalid"})
	if err == nil {
		t.Fatal("Apply accepted a play directory with no site.yml")
	}
	if !strings.Contains(err.Error(), "site.yml") {
		t.Fatalf("error does not name the entrypoint: %v", err)
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Fatal("the binary ran despite a missing entrypoint: argvFile exists")
	}
}

// TestRunRefusesAnEntrypointThatIsNotARegularFile is why the check is
// IsRegular rather than a bare Stat: a DIRECTORY named site.yml exists, and
// would otherwise reach ansible and fail there with the same unhelpful
// sentence this change exists to replace.
func TestRunRefusesAnEntrypointThatIsNotARegularFile(t *testing.T) {
	bin, argvFile, _ := fakeAnsiblePlaybook(t, `printf '%s' '`+jsonCallback+`'`)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "site.yml"), 0o700); err != nil {
		t.Fatalf("making a directory named site.yml: %v", err)
	}
	if _, err := (Runner{Bin: bin, Stderr: io.Discard}).Apply(context.Background(), dir, []string{"host1.invalid"}); err == nil {
		t.Fatal("Apply accepted a directory named site.yml")
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Fatal("the binary ran despite a non-file entrypoint: argvFile exists")
	}
}
