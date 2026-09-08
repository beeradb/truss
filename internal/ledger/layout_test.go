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

// TestKeyLayoutJoinsPrefixesButUsesSingletonKeysVerbatim: the per-commit
// records are <applied>/<sha> and <failed>/<sha>, exactly; HeadKey and
// HeartbeatKey are used verbatim with no prefix joined on, because they are
// configured as COMPLETE keys rather than as prefixes -- there is only ever
// one of each, so there is nothing to key them by.
func TestKeyLayoutJoinsPrefixesButUsesSingletonKeysVerbatim(t *testing.T) {
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

// TestDigestKeySlugReplacesEverySlash: the slug replaces every "/" in the
// root, not just the first -- so a nested root like "projects/recipes" must
// slug to "projects-recipes" and a deeper one to as many hyphens as it had
// slashes. Stopping at the first would collide two distinct roots onto one
// digest key.
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
