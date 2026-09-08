package parity

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRecordedCorpusMatchesTheLiveBash re-runs the capture against a real
// checkout of the reference repository and checks the committed corpus
// against what apply.sh does TODAY.
//
// ⚠️ IT EXISTS BECAUSE A GOLDEN FILE IS A CLAIM ABOUT SOMETHING THAT IS
// STILL RUNNING. The rest of this package compares truss against a
// recording; nothing in it can notice the recording going stale, and the
// bash applier keeps changing throughout the port -- that is the whole
// premise of the port plan. This is the only test here that could tell you
// the corpus is wrong rather than that truss is.
//
// Opt-in, and skipped by default, for two reasons that are facts about the
// environment rather than preferences: CI has no platform checkout and no
// bash applier to drive, and the reference repository is not this
// repository's to depend on.
//
//	TRUSS_PARITY_BASH=1 TRUSS_PLATFORM_REPO=/path/to/platform \
//	  go test ./internal/parity -run TestRecordedCorpusMatchesTheLiveBash
//
// It needs `uv` and the reference suite's own dependencies; see
// capture/README.md.
func TestRecordedCorpusMatchesTheLiveBash(t *testing.T) {
	if os.Getenv("TRUSS_PARITY_BASH") != "1" {
		t.Skip("set TRUSS_PARITY_BASH=1 and TRUSS_PLATFORM_REPO to check the corpus against the live bash")
	}
	repo := os.Getenv("TRUSS_PLATFORM_REPO")
	if repo == "" {
		t.Fatal("TRUSS_PARITY_BASH=1 needs TRUSS_PLATFORM_REPO pointing at a checkout of the reference repository")
	}

	captureDir, err := filepath.Abs("capture")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()

	// The reference suite has tests that fail for reasons of its own (its
	// workflow files, its commit hook), so the exit status is deliberately
	// not asserted on. What IS asserted is that every committed scenario
	// got a fresh recording: a capture that silently produced fewer is
	// exactly the vacuous pass this package's other test guards against.
	cmd := exec.Command("uv", "run", "--with", "pytest", "--with", "pyyaml",
		"python", "-m", "pytest", "tests/test_infra_pipeline.py", "-q", "-p", "capture_plugin")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(),
		"TRUSS_PARITY_OUT="+out,
		"PYTHONPATH="+captureDir+string(os.PathListSeparator)+filepath.Join(repo, "tests"),
	)
	output, runErr := cmd.CombinedOutput()

	fresh, err := Load(out)
	if err != nil {
		t.Fatalf("loading the fresh capture: %v\n\npytest said (exit %v):\n%s", err, runErr, output)
	}
	byName := map[string]Scenario{}
	for _, s := range fresh {
		byName[s.Name] = s
	}

	committed := loadCorpus(t)
	for _, want := range committed {
		got, ok := byName[want.Name]
		if !ok {
			t.Errorf("%s: the live bash produced no recording for this scenario", want.Name)
			continue
		}
		// Compared with the SAME machinery, so the two exceptions and the
		// day-count tolerance apply identically -- a bash-versus-bash
		// comparison hours apart drifts by exactly the clock, and by
		// nothing else.
		diffs := CompareOutcomes(want.Bash, got.Bash)
		unexplained, _ := Unexplained(want.Name, diffs, liveBashTolerances)
		if len(unexplained) > 0 {
			// The diff's two sides are labelled bash/truss because it
			// reuses the same comparison; here they are the RECORDING and
			// a FRESH RUN of the same bash, in that order.
			t.Errorf("%s: the committed recording no longer matches apply.sh -- re-capture, and read the diff "+
				"before you do. The diff's `bash` side is the recording; its `truss` side is what apply.sh "+
				"does now:%s", want.Name, Describe(unexplained))
		}
	}
	for name := range byName {
		if !inList(recordedScenarios, name) {
			t.Errorf("the live bash has a scenario %q the corpus does not: re-capture and add it to recordedScenarios", name)
		}
	}
}

// liveBashTolerances is the ONLY thing forgiven when comparing a recording
// against a fresh one of the same implementation: the day counts inside an
// alert, which move because the two runs read the clock at different
// moments. Everything the heartbeat carries is already handled by
// CompareOutcomes' own timestamp exception, so this list has one entry and
// should never grow -- a second entry would mean the bash changed and the
// corpus is being taught not to notice.
var liveBashTolerances = []Divergence{
	{
		ID:     "LIVE-BASH-CLOCK",
		Status: StatusIntended,
		Kind:   "alert",
		Accept: alertDiffersOnlyInTheExpiringClause,
		Bash:   "the recorded alert's day counts",
		Truss:  "a fresh run's, up to a day apart",
		Why:    "Two runs of the same script, hours apart, against fixtures written as an offset from now.",
		Ref:    "compare.go's dayCountTolerance",
	},
}
