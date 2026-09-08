package ledger

import "testing"

func testLayout() Layout {
	return Layout{
		AppliedPrefix:    "applier/applied",
		FailedPrefix:     "applier/failed",
		HeadKey:          "applier/head",
		HeartbeatKey:     "applier/heartbeat",
		PlanDigestPrefix: "applier/plan-digest",
	}
}

// TestKeyLayoutMatchesTheBash: <applied>/<sha> and <failed>/<sha>, exactly
// (apply.sh ledger_put_applied:334, ledger_put_failed:356-359); HeadKey and
// HeartbeatKey are used verbatim, with no prefix joined on, matching
// LEDGER_HEAD_KEY and HEARTBEAT_KEY being complete keys in apply.sh.
func TestKeyLayoutMatchesTheBash(t *testing.T) {
	l := testLayout()

	if got, want := l.AppliedKey("headsha1"), "applier/applied/headsha1"; got != want {
		t.Errorf("AppliedKey = %q, want %q", got, want)
	}
	if got, want := l.FailedKey("headsha1"), "applier/failed/headsha1"; got != want {
		t.Errorf("FailedKey = %q, want %q", got, want)
	}
	if l.HeadKey != "applier/head" {
		t.Errorf("HeadKey = %q, want the configured value verbatim", l.HeadKey)
	}
	if l.HeartbeatKey != "applier/heartbeat" {
		t.Errorf("HeartbeatKey = %q, want the configured value verbatim", l.HeartbeatKey)
	}
}

// TestDigestKeySlugReplacesEverySlash: apply.sh:637 pipes the root through
// `tr / -`, which replaces every "/" in the string, not just the first --
// so a nested root like "projects/recipes" must slug to "projects-recipes"
// and a deeper one to as many hyphens as it had slashes.
func TestDigestKeySlugReplacesEverySlash(t *testing.T) {
	l := testLayout()

	cases := []struct {
		root string
		want string
	}{
		{"credentials", "applier/plan-digest/headsha1/credentials.digest"},
		{"platform", "applier/plan-digest/headsha1/platform.digest"},
		{"projects/recipes", "applier/plan-digest/headsha1/projects-recipes.digest"},
		{"a/b/c", "applier/plan-digest/headsha1/a-b-c.digest"},
	}
	for _, tc := range cases {
		if got := l.DigestKey("headsha1", tc.root); got != tc.want {
			t.Errorf("DigestKey(%q) = %q, want %q", tc.root, got, tc.want)
		}
	}
}
