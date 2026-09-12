package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ---- fake tofu, run as this same test binary ----
//
// The pattern is the one os/exec's own tests use: the compiled test binary
// re-executes itself with GO_WANT_HELPER_PROCESS=1 in its (explicit) Env,
// and TestMain diverts to runFakeTofu instead of running go test's usual
// machinery. This needs no real `tofu`, no shell, and no separate build
// step — argv, cwd and env are exactly what the OS gave the child process.

const helperEnvVar = "GO_WANT_HELPER_PROCESS"

// testBinary is this compiled test binary's own path, used as Runner.Bin.
var testBinary string

func TestMain(m *testing.M) {
	if os.Getenv(helperEnvVar) == "1" {
		runFakeTofu()
		return // unreachable: every branch of runFakeTofu calls os.Exit
	}
	bin, err := os.Executable()
	if err != nil {
		panic(err)
	}
	testBinary = bin
	os.Exit(m.Run())
}

// fakeTofuRecord is what the fake tofu writes about how it was invoked.
type fakeTofuRecord struct {
	Argv []string `json:"argv"`
	Cwd  string   `json:"cwd"`
	Env  []string `json:"env"`
}

// runFakeTofu stands in for the real `tofu` binary. It is controlled
// entirely by environment variables — never by flags — because the argv it
// receives is exactly what Runner built and is itself under test.
func runFakeTofu() {
	rec := fakeTofuRecord{Argv: os.Args[1:], Env: os.Environ()}
	if cwd, err := os.Getwd(); err == nil {
		rec.Cwd = cwd
	}
	if path := os.Getenv("FAKE_TOFU_RECORD"); path != "" {
		if data, err := json.Marshal(rec); err == nil {
			_ = os.WriteFile(path, data, 0o600)
		}
	}

	switch os.Getenv("FAKE_TOFU_MODE") {
	case "lock-busy":
		os.Stderr.WriteString("Error: Error acquiring the state lock\n")
		os.Exit(1)
	case "fail":
		os.Stderr.WriteString("tofu blew up for a reason that is not a lock\n")
		os.Exit(1)
	case "drift":
		os.Stdout.WriteString("  # aws_instance.x will be updated in-place\n")
		os.Exit(2)
	case "show-json":
		os.Stdout.WriteString(os.Getenv("FAKE_TOFU_SHOWJSON"))
		os.Exit(0)
	case "stdout-noise":
		os.Stdout.WriteString("chatty line that must never reach the real stdout\n")
		os.Exit(0)
	default:
		os.Stdout.WriteString("ok\n")
		os.Exit(0)
	}
}

// fakeSetup bundles a Runner wired at the fake tofu, the path it will
// record its invocation to, and the buffer standing in for Runner.Stderr.
type fakeSetup struct {
	Runner Runner
	Record string
	Stderr *bytes.Buffer
}

func newFake(t *testing.T, mode string, extraEnv ...string) fakeSetup {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record.json")
	var stderr bytes.Buffer
	env := []string{
		helperEnvVar + "=1",
		"FAKE_TOFU_MODE=" + mode,
		"FAKE_TOFU_RECORD=" + record,
	}
	env = append(env, extraEnv...)
	return fakeSetup{
		Runner: Runner{
			Bin:       testBinary,
			PluginDir: "/opt/tofu-providers",
			Stderr:    &stderr,
			Env:       env,
		},
		Record: record,
		Stderr: &stderr,
	}
}

func (fs fakeSetup) readRecord(t *testing.T) fakeTofuRecord {
	t.Helper()
	data, err := os.ReadFile(fs.Record)
	if err != nil {
		t.Fatalf("fake tofu never recorded an invocation: %v", err)
	}
	var rec fakeTofuRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("could not parse fake tofu record: %v", err)
	}
	return rec
}

func recordExists(fs fakeSetup) bool {
	_, err := os.Stat(fs.Record)
	return err == nil
}

func argvContains(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func argvHasPrefix(argv []string, prefix string) bool {
	for _, a := range argv {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// ---- TestInitAlwaysPassesPluginDirAndReadonlyLockfile ----

func TestInitAlwaysPassesPluginDirAndReadonlyLockfile(t *testing.T) {
	dir := t.TempDir()

	for _, pluginDir := range []string{"/opt/tofu-providers", "/other/plugins"} {
		fs := newFake(t, "ok")
		fs.Runner.PluginDir = pluginDir

		if err := fs.Runner.Init(context.Background(), dir); err != nil {
			t.Fatalf("Init: %v", err)
		}
		rec := fs.readRecord(t)

		if len(rec.Argv) == 0 || rec.Argv[0] != "init" {
			t.Fatalf("argv did not start with init: %v", rec.Argv)
		}
		if !argvContains(rec.Argv, "-plugin-dir="+pluginDir) {
			t.Errorf("init argv %v missing -plugin-dir=%s", rec.Argv, pluginDir)
		}
		if !argvContains(rec.Argv, "-lockfile=readonly") {
			t.Errorf("init argv %v missing -lockfile=readonly", rec.Argv)
		}
	}
}

// ---- TestNoInvocationCanReachARegistry ----

func TestNoInvocationCanReachARegistry(t *testing.T) {
	// There is no parameter or code path that can build an init argv
	// without both flags: -plugin-dir pins providers to the image's own
	// directory and -lockfile=readonly forbids init from resolving a
	// different provider set than the one reviewed in the commit.
	dir := t.TempDir()
	fs := newFake(t, "ok")

	if err := fs.Runner.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	rec := fs.readRecord(t)

	if !argvHasPrefix(rec.Argv, "-plugin-dir=") {
		t.Errorf("init argv %v carries no -plugin-dir flag at all", rec.Argv)
	}
	if !argvContains(rec.Argv, "-lockfile=readonly") {
		t.Errorf("init argv %v carries no -lockfile=readonly flag at all", rec.Argv)
	}
	// An empty -plugin-dir is still an explicit flag, not a fallback to a
	// registry: the flag is always present, whatever value PluginDir holds.
	fs2 := newFake(t, "ok")
	fs2.Runner.PluginDir = ""
	if err := fs2.Runner.Init(context.Background(), dir); err != nil {
		t.Fatalf("Init with empty PluginDir: %v", err)
	}
	rec2 := fs2.readRecord(t)
	if !argvContains(rec2.Argv, "-plugin-dir=") {
		t.Errorf("init argv %v dropped -plugin-dir when PluginDir was empty", rec2.Argv)
	}
}

// ---- TestApplyOnlyEverAppliesAPlanFile ----

func TestApplyOnlyEverAppliesAPlanFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("refuses an empty plan file without ever invoking tofu", func(t *testing.T) {
		fs := newFake(t, "ok")
		err := fs.Runner.Apply(context.Background(), dir, "")
		if err == nil {
			t.Fatal("Apply with no plan file returned nil error")
		}
		if recordExists(fs) {
			t.Fatal("Apply with no plan file still ran tofu")
		}
	})

	t.Run("applies exactly the named plan file, and never -auto-approve", func(t *testing.T) {
		fs := newFake(t, "ok")
		if err := fs.Runner.Apply(context.Background(), dir, "tfplan"); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		rec := fs.readRecord(t)
		want := []string{"apply", "-input=false", "-no-color", "tfplan"}
		if !equalStrings(rec.Argv, want) {
			t.Fatalf("apply argv = %v, want %v", rec.Argv, want)
		}
		if argvContains(rec.Argv, "-auto-approve") {
			t.Fatal("apply argv carries -auto-approve")
		}
	})
}

// ---- TestPlanOutputNeverReachesStdout ----

func TestPlanOutputNeverReachesStdout(t *testing.T) {
	dir := t.TempDir()
	fs := newFake(t, "stdout-noise")

	// Swap the real os.Stdout for a pipe so a regression that connects
	// tofu's stdout straight through (instead of to Runner.Stderr) would
	// show up here rather than merely in a captured buffer this test
	// controls end to end.
	realStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	runErr := fs.Runner.Plan(context.Background(), dir, "tfplan")

	os.Stdout = realStdout
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	var captured bytes.Buffer
	if _, err := captured.ReadFrom(r); err != nil {
		t.Fatalf("read pipe: %v", err)
	}

	if runErr != nil {
		t.Fatalf("Plan: %v", runErr)
	}
	if strings.Contains(captured.String(), "chatty line") {
		t.Fatalf("tofu's stdout reached the real os.Stdout: %q", captured.String())
	}
	if !strings.Contains(fs.Stderr.String(), "chatty line") {
		t.Fatalf("tofu's stdout was not forwarded to Runner.Stderr: %q", fs.Stderr.String())
	}
}

// ---- TestALockedStateIsContentionNotFailure ----

func TestALockedStateIsContentionNotFailure(t *testing.T) {
	dir := t.TempDir()

	t.Run("Init", func(t *testing.T) {
		fs := newFake(t, "lock-busy")
		err := fs.Runner.Init(context.Background(), dir)
		if !errors.Is(err, ErrLockBusy) {
			t.Fatalf("Init error = %v, want ErrLockBusy", err)
		}
	})
	t.Run("Plan", func(t *testing.T) {
		fs := newFake(t, "lock-busy")
		err := fs.Runner.Plan(context.Background(), dir, "tfplan")
		if !errors.Is(err, ErrLockBusy) {
			t.Fatalf("Plan error = %v, want ErrLockBusy", err)
		}
	})
	t.Run("Apply", func(t *testing.T) {
		fs := newFake(t, "lock-busy")
		err := fs.Runner.Apply(context.Background(), dir, "tfplan")
		if !errors.Is(err, ErrLockBusy) {
			t.Fatalf("Apply error = %v, want ErrLockBusy", err)
		}
	})
	t.Run("a generic failure is never mistaken for a busy lock", func(t *testing.T) {
		fs := newFake(t, "fail")
		err := fs.Runner.Apply(context.Background(), dir, "tfplan")
		if err == nil {
			t.Fatal("Apply returned nil error for a failing tofu")
		}
		if errors.Is(err, ErrLockBusy) {
			t.Fatalf("a plain failure was reported as ErrLockBusy: %v", err)
		}
	})
}

// ---- TestDetailedExitcodeTwoIsDriftNotAnError ----

func TestDetailedExitcodeTwoIsDriftNotAnError(t *testing.T) {
	dir := t.TempDir()

	t.Run("exit 2 is drift, not an error", func(t *testing.T) {
		fs := newFake(t, "drift")
		changes, err := fs.Runner.PlanDetailed(context.Background(), dir)
		if err != nil {
			t.Fatalf("PlanDetailed: %v", err)
		}
		if !changes {
			t.Fatal("PlanDetailed reported no changes for exit code 2")
		}
	})
	t.Run("exit 0 is clean", func(t *testing.T) {
		fs := newFake(t, "ok")
		changes, err := fs.Runner.PlanDetailed(context.Background(), dir)
		if err != nil {
			t.Fatalf("PlanDetailed: %v", err)
		}
		if changes {
			t.Fatal("PlanDetailed reported changes for exit code 0")
		}
	})
	t.Run("any other exit code is a real error", func(t *testing.T) {
		fs := newFake(t, "fail")
		changes, err := fs.Runner.PlanDetailed(context.Background(), dir)
		if err == nil {
			t.Fatal("PlanDetailed returned nil error for exit code 1")
		}
		if changes {
			t.Fatal("PlanDetailed reported changes alongside an error")
		}
	})
	t.Run("a busy lock during drift is still contention", func(t *testing.T) {
		fs := newFake(t, "lock-busy")
		_, err := fs.Runner.PlanDetailed(context.Background(), dir)
		if !errors.Is(err, ErrLockBusy) {
			t.Fatalf("PlanDetailed error = %v, want ErrLockBusy", err)
		}
	})
}

// ---- TestDriftPlanNeverWrites ----

func TestDriftPlanNeverWrites(t *testing.T) {
	dir := t.TempDir()
	fs := newFake(t, "ok")

	if _, err := fs.Runner.PlanDetailed(context.Background(), dir); err != nil {
		t.Fatalf("PlanDetailed: %v", err)
	}
	rec := fs.readRecord(t)

	if argvHasPrefix(rec.Argv, "-out") {
		t.Fatalf("drift plan argv %v carries -out: it would write a plan file", rec.Argv)
	}
	if !argvContains(rec.Argv, "-detailed-exitcode") {
		t.Errorf("drift plan argv %v missing -detailed-exitcode", rec.Argv)
	}
	if !argvContains(rec.Argv, "-lock=false") {
		t.Errorf("drift plan argv %v missing -lock=false", rec.Argv)
	}
}

// ---- TestShowJSONAlwaysNamesThePlanFile ----

func TestShowJSONAlwaysNamesThePlanFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("refuses an empty plan file without ever invoking tofu", func(t *testing.T) {
		fs := newFake(t, "ok")
		_, err := fs.Runner.ShowJSON(context.Background(), dir, "")
		if err == nil {
			t.Fatal("ShowJSON with no plan file returned nil error")
		}
		if recordExists(fs) {
			t.Fatal("ShowJSON with no plan file still ran tofu")
		}
	})

	t.Run("names the plan file and returns exactly what tofu printed", func(t *testing.T) {
		const want = `{"resource_changes":[{"address":"a"}]}`
		fs := newFake(t, "show-json", "FAKE_TOFU_SHOWJSON="+want)
		out, err := fs.Runner.ShowJSON(context.Background(), dir, "tfplan")
		if err != nil {
			t.Fatalf("ShowJSON: %v", err)
		}
		if string(out) != want {
			t.Fatalf("ShowJSON = %q, want %q", out, want)
		}
		rec := fs.readRecord(t)
		wantArgv := []string{"show", "-json", "tfplan"}
		if !equalStrings(rec.Argv, wantArgv) {
			t.Fatalf("show argv = %v, want %v", rec.Argv, wantArgv)
		}
	})
}

// ---- TestTheEnvironmentIsExactlyWhatWasGiven ----

func TestTheEnvironmentIsExactlyWhatWasGiven(t *testing.T) {
	dir := t.TempDir()

	t.Run("the child sees exactly Runner.Env, nothing more and nothing less", func(t *testing.T) {
		fs := newFake(t, "ok", "MARKER=only-this-run-should-have-it")
		if err := fs.Runner.Init(context.Background(), dir); err != nil {
			t.Fatalf("Init: %v", err)
		}
		rec := fs.readRecord(t)

		gotEnv := append([]string(nil), rec.Env...)
		wantEnv := append([]string(nil), fs.Runner.Env...)
		sort.Strings(gotEnv)
		sort.Strings(wantEnv)
		if !equalStrings(gotEnv, wantEnv) {
			t.Fatalf("child env = %v, want exactly %v", gotEnv, wantEnv)
		}

		// The parent test process has its own PATH/HOME/etc; none of that
		// may leak into the child merely because Env did not mention it.
		for _, kv := range rec.Env {
			if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "HOME=") {
				t.Fatalf("child env carries an ambient variable it was never given: %s", kv)
			}
		}
	})

	t.Run("a zero-value Env never falls back to exec.Cmd's nil-means-inherit default", func(t *testing.T) {
		if got := explicitEnv(nil); got == nil {
			t.Fatal("explicitEnv(nil) returned nil: exec.Cmd would inherit the ambient environment")
		} else if len(got) != 0 {
			t.Fatalf("explicitEnv(nil) = %v, want empty", got)
		}

		given := []string{"A=1", "B=2"}
		if got := explicitEnv(given); !equalStrings(got, given) {
			t.Fatalf("explicitEnv(%v) = %v, want unchanged", given, got)
		}
	})

	t.Run("exec.Cmd itself would inherit on a nil Env, which is exactly what explicitEnv prevents", func(t *testing.T) {
		// Documents the gotcha explicitEnv exists to close: this is a plain
		// exec.Cmd, not a Runner, with Env left nil.
		cmd := exec.CommandContext(context.Background(), "true")
		if cmd.Env != nil {
			t.Fatal("test assumption violated: exec.Cmd.Env was not nil by default")
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAFailureReasonIsNotATranscript guards the security property
// apply.sh:170-180 records under its own "Security review, 2026-09-07", and
// that this port reintroduced: the error wrapExecError returns becomes the
// pass's failure reason, which is written to a ledger object anyone holding
// the bucket credential can read AND sent to a Telegram chat.
//
// A provider makes no promise about what it prints in an error -- a request
// body, a resource attribute, a token. OpenTofu redacts what it knows to be
// sensitive and nothing more. Found again by internal/parity on 2026-09-08,
// which printed it as a diff against the bash.
func TestAFailureReasonIsNotATranscript(t *testing.T) {
	const marker = "SENSITIVE-PROVIDER-OUTPUT-MARKER"

	dir := t.TempDir()
	bin := filepath.Join(dir, "tofu")
	script := "#!/bin/sh\necho '" + marker + "'\necho 'Bearer aaaaaaaaaaaaaaaa'\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("writing stub tofu: %v", err)
	}

	var podLog bytes.Buffer
	r := Runner{Bin: bin, Stderr: &podLog}

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"init", func() error { return r.Init(context.Background(), dir) }},
		{"plan", func() error { return r.Plan(context.Background(), dir, "tfplan") }},
		{"apply", func() error { return r.Apply(context.Background(), dir, "tfplan") }},
		{"plan-detailed", func() error { _, err := r.PlanDetailed(context.Background(), dir); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			podLog.Reset()
			err := tc.call()
			if err == nil {
				t.Fatal("no error from a stub that exits 1, so this test proves nothing")
			}
			if strings.Contains(err.Error(), marker) {
				t.Errorf("the failure reason carries tofu's output, which reaches the ledger and Telegram:\n%s", err)
			}
			if strings.Contains(err.Error(), dir) {
				t.Errorf("the failure reason carries the pod's working directory:\n%s", err)
			}
			// The real property: a reason is a short sentence, never a
			// transcript. Every caller prefixes the step and the root
			// ("tofu plan failed for %s: %v"), so what belongs here is the
			// bare cause. A newline or a long body means output leaked back
			// in, whatever it happens to contain.
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("the failure reason spans lines, so it is a transcript: %q", err)
			}
			if len(err.Error()) > 120 {
				t.Errorf("the failure reason is %d bytes; a reason is a sentence, not a body:\n%s", len(err.Error()), err)
			}
			// And nothing is lost: the transcript is in the pod log.
			if !strings.Contains(podLog.String(), marker) {
				t.Errorf("the transcript did not reach the pod log, so dropping it from the reason DOES lose it:\n%s", podLog.String())
			}
		})
	}
}

func TestForceUnlockRefusesWithoutALockIDWithoutEverInvokingTofu(t *testing.T) {
	fs := newFake(t, "ok")
	err := fs.Runner.ForceUnlock(context.Background(), t.TempDir(), "")
	if err == nil {
		t.Fatal("ForceUnlock with no lock ID returned nil error")
	}
	if recordExists(fs) {
		t.Fatal("ForceUnlock with no lock ID still ran tofu")
	}
}

func TestForceUnlockBuildsTheExactArgv(t *testing.T) {
	fs := newFake(t, "ok")
	dir := t.TempDir()
	if err := fs.Runner.ForceUnlock(context.Background(), dir, "abc-123"); err != nil {
		t.Fatalf("ForceUnlock: %v", err)
	}
	// The fake tofu overwrites FAKE_TOFU_RECORD on every invocation, so the
	// record left behind after ForceUnlock returns is its LAST exec: the
	// force-unlock itself, run after Init succeeded.
	rec := fs.readRecord(t)
	want := []string{"force-unlock", "-force", "abc-123"}
	if !equalStrings(rec.Argv, want) {
		t.Fatalf("force-unlock argv = %v, want %v", rec.Argv, want)
	}
}

func TestForceUnlocksErrorCarriesNoTofuTranscript(t *testing.T) {
	fs := newFake(t, "fail")
	err := fs.Runner.ForceUnlock(context.Background(), t.TempDir(), "abc-123")
	if err == nil {
		t.Fatal("ForceUnlock with a failing tofu returned nil error")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("the failure reason spans lines, so it is a transcript: %q", err)
	}
}
