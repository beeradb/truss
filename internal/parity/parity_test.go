package parity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const scenarioDir = "testdata/scenarios"

// TestPassAgreesWithBashOnEveryFixture is docs/port-plan.md §5.5's first
// named test: one subtest per recorded scenario, each asserting that the
// final bucket and the alert text truss produces equal the ones apply.sh
// produced, with the two documented exceptions (root ordering inside
// applied/<sha>, §3.4; and timestamps) and the enumerated divergence
// allowlist in divergences.go. Anything else is a failure.
func TestPassAgreesWithBashOnEveryFixture(t *testing.T) {
	scenarios := loadCorpus(t)

	dir := t.TempDir()
	h, err := Build(dir)
	if err != nil {
		t.Fatalf("building the harness: %v", err)
	}

	usedIDs := map[string]bool{}
	ran := 0
	for _, s := range scenarios {
		t.Run(s.Name, func(t *testing.T) {
			ran++
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			got, stderr, err := h.Run(ctx, s, time.Now())
			if err != nil {
				t.Fatalf("running truss: %v\nstderr:\n%s", err, stderr)
			}

			diffs := CompareOutcomes(s.Bash, got)
			unexplained, used := Unexplained(s.Name, diffs, Divergences)
			for _, id := range used {
				usedIDs[id] = true
			}
			if len(unexplained) > 0 {
				t.Errorf("truss and apply.sh disagree, and no entry in divergences.go accounts for it:%s\n\ntruss stderr:\n%s",
					Describe(unexplained), stderr)
			}
		})
	}

	// ⚠️ AN ALLOWLIST ENTRY THAT MATCHES NOTHING IS A HOLE, NOT A SPARE.
	// It would silently accept the difference it names if that difference
	// ever came back, and nothing would say so. The same standing rule the
	// rest of this repo applies to a guard nobody has watched fail.
	//
	// Only when every scenario ran: `go test -run .../one_subtest` is how
	// anybody debugs one failure, and reporting all eight entries as holes
	// on top of the one real failure would bury it. Found the first time
	// this check was negative-tested.
	if ran != len(scenarios) {
		t.Logf("%d of %d scenarios ran; skipping the unused-divergence check", ran, len(scenarios))
		return
	}
	// ⚠️ EVERY RUN SAYS OUT LOUD WHAT IT IS FORGIVING, AND WHICH OF THOSE
	// NOBODY APPROVED. A green parity run over two implementations that
	// differ is only honest if the differences are visible; printed here,
	// `go test -v ./internal/parity` is a readable statement of them
	// rather than a bare "ok".
	for _, dv := range Divergences {
		if dv.Status == StatusFinding {
			t.Logf("FORGIVEN, AND REPORTED AS A DEFECT -- %s: bash %s; truss %s (%s)", dv.ID, dv.Bash, dv.Truss, dv.Ref)
		}
	}

	for _, dv := range Divergences {
		if !usedIDs[dv.ID] {
			t.Errorf("divergence %q matched no diff in any scenario: either it has been fixed (delete the entry) "+
				"or the corpus no longer reaches it (say so, and why)", dv.ID)
		}
	}
}

// TestEveryBashScenarioHasAGoSubtest is §5.5's second named test, and it
// guards the corpus rather than the code.
//
// ⚠️ WHAT IT PROTECTS AGAINST IS THE CORPUS SHRINKING QUIETLY. A parity
// harness whose corpus lost half its scenarios still passes, and reports
// success -- which is the vacuous-pass shape this project has a standing
// rule against. So the set of scenarios is pinned here as an explicit
// manifest: adding one, or losing one, is an edit somebody has to make on
// purpose.
func TestEveryBashScenarioHasAGoSubtest(t *testing.T) {
	scenarios := loadCorpus(t)

	got := map[string]bool{}
	for _, s := range scenarios {
		got[s.Name] = true
	}

	for _, want := range recordedScenarios {
		if !got[want] {
			t.Errorf("scenario %q is in the manifest and not in %s: the corpus has shrunk, "+
				"or a capture run failed part-way", want, scenarioDir)
		}
	}
	for name := range got {
		if !inList(recordedScenarios, name) {
			t.Errorf("scenario %q is in %s and not in the manifest: add it to recordedScenarios "+
				"so losing it later is visible", name, scenarioDir)
		}
	}
	if len(recordedScenarios) != len(got) {
		t.Errorf("manifest has %d scenarios, corpus has %d", len(recordedScenarios), len(got))
	}
}

func loadCorpus(t *testing.T) []Scenario {
	t.Helper()
	abs, err := filepath.Abs(scenarioDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("no recorded corpus at %s -- see capture/README.md: %v", abs, err)
	}
	scenarios, err := Load(abs)
	if err != nil {
		t.Fatalf("loading the corpus: %v", err)
	}
	if len(scenarios) == 0 {
		t.Fatalf("the corpus at %s is empty", abs)
	}
	return scenarios
}

func inList(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestEmptyPlanIsNotGatedAcceptsOnlyItsOwnStory is the negative half of the
// EMPTY-PLAN-IS-NOT-GATED entry. Its heartbeat case once ended in
// `default: return true`, so any unrelated heartbeat difference in its two
// scenarios was forgiven under a divergence that claims to be pinned to one
// story -- a divergence list that accepts everything says nothing.
func TestEmptyPlanIsNotGatedAcceptsOnlyItsOwnStory(t *testing.T) {
	accepted := []Diff{
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/failure",
			Bash: "projects/recipes: does not match the one approved at headsha1", Truss: ""},
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/applied", Bash: "0", Truss: "1"},
		{Kind: "value", Key: "applied/HEAD", Bash: "base", Truss: "sha1"},
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/last_sha", Bash: "base", Truss: "sha1"},
	}
	for _, d := range accepted {
		if !emptyPlanIsNotGated(d) {
			t.Errorf("%s %s is part of this story and was refused", d.Key, d.Path)
		}
	}

	refused := []Diff{
		// An unrelated field on the same object: exactly what the old
		// default waved through.
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/noop", Bash: "0", Truss: "7"},
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/expiring", Bash: "[]", Truss: `["cf-token-mint"]`},
		// The right path, the wrong direction: truss refusing where the
		// bash applied is not this divergence.
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/applied", Bash: "1", Truss: "0"},
		// A failure that does not name the digest gate.
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/failure", Bash: "tofu apply failed", Truss: ""},
		// The watermark moved to a sha that is not the commit this story is
		// about is the regression this key exists to catch.
		{Kind: "value", Key: "applied/HEAD", Bash: "base", Truss: "someothersha"},
		{Kind: "value", Key: "applied/HEAD", Bash: "base", Truss: "<absent>"},
		// truss recording no last_sha at all, or the WRONG one, is not "the
		// queue advanced past the commit the bash refused".
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/last_sha", Bash: "base", Truss: "<absent>"},
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/last_sha", Bash: "base", Truss: ""},
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/last_sha", Bash: "base", Truss: "someothersha"},
		{Kind: "value", Key: "heartbeat/applier.json", Path: "/last_sha", Bash: "sha1", Truss: "base"},
	}
	for _, d := range refused {
		if emptyPlanIsNotGated(d) {
			t.Errorf("%s %s = (%q, %q) is not this story and was accepted", d.Key, d.Path, d.Bash, d.Truss)
		}
	}
}
