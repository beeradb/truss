package notify

import "testing"

func intp(n int) *int { return &n }

func TestNothingToApplyMessage(t *testing.T) {
	got := Compose(Report{Subject: "platform applier", LastSHA: "sha1"})
	want := "platform applier: nothing to apply"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}

func TestAppliedAndNoopCountsAreInTheMessage(t *testing.T) {
	got := Compose(Report{Subject: "platform applier", LastSHA: "sha1", Applied: 3, Noop: 5})
	want := "platform applier: applied=3 noop=5 last=sha1"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}

func TestAFailureLeadsWithFAILEDAndTheTrimmedReason(t *testing.T) {
	got := Compose(Report{
		Subject: "platform applier", LastSHA: "sha1",
		Applied: 1, Noop: 2, Failure: "exit code 1\x00 from apply",
	})
	want := "platform applier FAILED at sha1: exit code 1 from apply (applied=1 noop=2)"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}

	long := strings200(801)
	got = Compose(Report{Subject: "platform applier", LastSHA: "sha1", Failure: long})
	marker := "\n... truncated; see the run's pod logs for the rest."
	wantSuffix := marker + " (applied=0 noop=0)"
	if !hasSuffix(got, wantSuffix) {
		t.Fatalf("Compose() = %q, want it to end with the truncation marker", got)
	}
	if len(got) != 800+len(wantSuffix)+len("platform applier FAILED at sha1: ") {
		t.Fatalf("Compose() length = %d, reason was not cut at 800 bytes", len(got))
	}
}

func TestASkippedDriftSaysSoRatherThanLookingClean(t *testing.T) {
	got := Compose(Report{
		Subject: "platform applier", LastSHA: "sha1",
		DriftRun: true, DriftSkipped: "ExternalSecret not synced",
	})
	want := "platform applier: nothing to apply; DRIFT NOT CHECKED: ExternalSecret not synced"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}

	// Not a drift run at all: the clause must not appear even if DriftSkipped
	// happens to be set.
	got = Compose(Report{Subject: "platform applier", LastSHA: "sha1", DriftSkipped: "ignored"})
	want = "platform applier: nothing to apply"
	if got != want {
		t.Fatalf("Compose() = %q, want %q: a non-drift run must not mention a skip", got, want)
	}
}

func TestDriftIsNamedNeverCounted(t *testing.T) {
	got := Compose(Report{
		Subject: "platform applier", LastSHA: "sha1",
		Drifted: []string{"platform", "projects/alpha"},
	})
	want := "platform applier: nothing to apply; DRIFT: platform, projects/alpha differ from the code"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}

func TestDriftUnknownIsDistinctFromDrift(t *testing.T) {
	got := Compose(Report{
		Subject: "platform applier", LastSHA: "sha1",
		Drifted: []string{"platform"},
		Errored: []string{"projects/alpha"},
	})
	want := "platform applier: nothing to apply; DRIFT: platform differ from the code; drift UNKNOWN for: projects/alpha"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}

func TestExpiringNamesEachCredentialAndItsDays(t *testing.T) {
	got := Compose(Report{
		Subject: "platform applier", LastSHA: "sha1",
		Expiring: []Expiring{
			{Name: "credential-a", DaysLeft: intp(12)},
			{Name: "credential-b", DaysLeft: nil},
		},
	})
	want := "platform applier: nothing to apply; EXPIRING: credential-a in 12d, credential-b (no expiry recorded)"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}

func TestRotationIsMentionedOnlyWhenSomethingChanged(t *testing.T) {
	got := Compose(Report{Subject: "platform applier", LastSHA: "sha1", RotatedChanges: 0})
	want := "platform applier: nothing to apply"
	if got != want {
		t.Fatalf("Compose() = %q, want %q: zero changes must not be mentioned", got, want)
	}

	got = Compose(Report{Subject: "platform applier", LastSHA: "sha1", RotatedChanges: 2})
	want = "platform applier: nothing to apply; rotated credentials (2 changes)"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}

func TestClauseOrderIsFixed(t *testing.T) {
	got := Compose(Report{
		Subject: "platform applier", LastSHA: "sha1", Applied: 1, Noop: 1,
		DriftRun: true, DriftSkipped: "mount missing",
		RotatedChanges: 4,
		Drifted:        []string{"platform"},
		Errored:        []string{"projects/alpha"},
		Expiring:       []Expiring{{Name: "credential-a", DaysLeft: intp(1)}},
	})
	want := "platform applier: applied=1 noop=1 last=sha1" +
		"; DRIFT NOT CHECKED: mount missing" +
		"; rotated credentials (4 changes)" +
		"; DRIFT: platform differ from the code" +
		"; drift UNKNOWN for: projects/alpha" +
		"; EXPIRING: credential-a in 1d"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}

func strings200(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
