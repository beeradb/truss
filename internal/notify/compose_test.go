package notify

import (
	"reflect"
	"testing"
)

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
		Subject: "platform applier", LastSHA: "base", FailedSHA: "sha1",
		Applied: 1, Noop: 2, Failure: "exit code 1\x00 from apply",
	})
	want := "platform applier FAILED at sha1: exit code 1 from apply (applied=1 noop=2)"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}

	long := strings200(801)
	got = Compose(Report{Subject: "platform applier", LastSHA: "base", FailedSHA: "sha1", Failure: long})
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

// TestSilentAgreesWithComposeOnEveryField holds Silent and Compose together:
// silent must mean "Compose wrote the idle headline and nothing after it",
// field by field.
//
// ⚠️ REFLECTION RATHER THAN A LIST, because the list is what went wrong. The
// caller asked the narrower question -- did anything apply -- and every daily
// drift pass answers no while carrying DRIFT, EXPIRING and EXPIRY NOT CHECKED
// clauses, so the one report worth reading was discarded while the monitor
// stayed green. A clause added to Compose with no matching term in Silent
// fails here now, and a field added with no sample fails too rather than
// passing unexamined.
func TestSilentAgreesWithComposeOnEveryField(t *testing.T) {
	// DriftRun is set here because DriftSkipped only speaks on a drift pass;
	// it is a pair, and the pair is exercised through DriftSkipped below.
	idle := Report{Subject: "platform applier", LastSHA: "abc1234", DriftRun: true}
	idleText := Compose(idle)
	if idleText != "platform applier: nothing to apply" {
		t.Fatalf("idle headline changed: %q", idleText)
	}
	if !idle.Silent() {
		t.Fatalf("a report with nothing in it is not silent")
	}

	days := 3
	samples := map[string]any{
		"Applied":           1,
		"Noop":              1,
		"Failure":           "tofu apply exploded",
		"DriftSkipped":      "the protection gate failed",
		"Drifted":           []string{"projects/paperless"},
		"Errored":           []string{"projects/immich"},
		"RotatedChanges":    2,
		"Expiring":          []Expiring{{Name: "cf-token-mint", DaysLeft: &days}},
		"ExpiryUnavailable": "vault said no",
	}

	rt := reflect.TypeOf(Report{})
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		switch name {
		case "Subject", "LastSHA", "FailedSHA", "PlannedSHA", "DriftRun":
			// Not clauses. Subject and LastSHA render the headline;
			// DriftRun gates another field; FailedSHA and PlannedSHA only
			// change the headline of a report that ALSO carries a Failure,
			// and Failure's own sample below is what exercises Silent for
			// all three. Their own behaviour is pinned by
			// TestAFailureNamesTheCommitThatFailedNotTheLedgerPosition.
			continue
		}
		sample, ok := samples[name]
		if !ok {
			t.Fatalf("Report.%s has no sample here: add one, and a term in Silent if it adds a clause", name)
		}
		r := idle
		reflect.ValueOf(&r).Elem().FieldByName(name).Set(reflect.ValueOf(sample))

		text := Compose(r)
		if text == idleText {
			t.Errorf("Report.%s = %v changed nothing in Compose; the sample does not exercise it", name, sample)
			continue
		}
		if r.Silent() {
			t.Errorf("Report.%s = %v makes Compose say %q, and Silent still reports nothing to say", name, sample, text)
		}
	}
}

// TestAFailureNamesTheCommitThatFailedNotTheLedgerPosition is the
// reproduction of a defect the reference bash has and truss copied: the
// failure clause printed the LEDGER POSITION -- the last commit that
// succeeded -- under the words "FAILED at <sha>". A refusal of the NEXT
// commit therefore pointed a reader at the previous one.
//
// Observed on a live applier 2026-09-10: "FAILED at f42f97f" about a
// `provider "tailscale" {}` block that exists only in the commit after
// f42f97f. The person debugging it had to diff two trees to discover the
// alert was talking about neither.
//
// ⚠️ LastSHA IS DELIBERATELY DIFFERENT FROM FailedSHA IN EVERY CASE BELOW.
// A fixture where the two agree cannot tell the fix from the defect -- that
// is exactly the shape the original bug hid in, because most passes apply
// nothing and the two shas coincide.
func TestAFailureNamesTheCommitThatFailedNotTheLedgerPosition(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    Report
		want string
	}{
		{
			name: "the commit that failed, not the one that last succeeded",
			r: Report{
				Subject: "platform applier", LastSHA: "base", FailedSHA: "sha1",
				Failure: "tofu plan failed for platform",
			},
			want: "platform applier FAILED at sha1: tofu plan failed for platform (applied=0 noop=0)",
		},
		{
			// The applier plans at the BRANCH HEAD while working through
			// the queue one commit at a time, so the tree that produced a
			// tofu error is not in general the tree of the commit whose
			// turn it was. Naming only one of the two is what made the live
			// case take a two-tree diff to explain.
			name: "and the tree it planned, when that is a different one",
			r: Report{
				Subject: "platform applier", LastSHA: "base",
				FailedSHA: "sha1", PlannedSHA: "headsha1",
				Failure: "tofu plan failed for platform",
			},
			want: "platform applier FAILED at sha1 (planned at headsha1): tofu plan failed for platform (applied=0 noop=0)",
		},
		{
			// A queue holding one commit plans that commit's own tree, and
			// repeating the sha would be noise rather than information.
			name: "but not when the head and the commit are the same",
			r: Report{
				Subject: "platform applier", LastSHA: "base",
				FailedSHA: "sha1", PlannedSHA: "sha1",
				Failure: "tofu plan failed for platform",
			},
			want: "platform applier FAILED at sha1: tofu plan failed for platform (applied=0 noop=0)",
		},
		{
			// ⚠️ NO COMMIT IS NAMED, AND THAT IS THE POINT. A protection
			// gate refuses before the queue is even read, so there is no
			// commit this failure belongs to. The bash printed one anyway
			// -- plausible, and irrelevant.
			name: "no commit at all when the failure belongs to none",
			r: Report{
				Subject: "platform applier", LastSHA: "base",
				Failure: "branch protection on main does not meet the bar: enforce_admins is off",
			},
			want: "platform applier FAILED: branch protection on main does not meet the bar: enforce_admins is off (applied=0 noop=0)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Compose(tc.r); got != tc.want {
				t.Fatalf("Compose() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestASuccessfulPassStillReportsTheLedgerPosition guards the other half:
// LastSHA is still the right answer for the non-failure summary, and
// narrowing the failure clause must not have narrowed that too.
func TestASuccessfulPassStillReportsTheLedgerPosition(t *testing.T) {
	got := Compose(Report{Subject: "platform applier", LastSHA: "sha9", Applied: 2, Noop: 1})
	want := "platform applier: applied=2 noop=1 last=sha9"
	if got != want {
		t.Fatalf("Compose() = %q, want %q", got, want)
	}
}
