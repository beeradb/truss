package gates

import (
	"strings"
	"testing"
)

const (
	renderUnit = "deliveries/beta/web"
	renderHead = "0123456789abcdef0123456789abcdef01234567"
	renderKey  = "digests/0123456789abcdef0123456789abcdef01234567/deliveries-beta-web.digest"
)

func TestCheckRenderDigestAcceptsAMatch(t *testing.T) {
	if got := CheckRenderDigest(renderUnit, renderHead, renderKey, "aaaa", "aaaa", true); len(got) != 0 {
		t.Fatalf("a matching render was refused: %v", got)
	}
}

// TestCheckRenderDigestRefusesAnUnrecordedRender covers the case CI never
// filed anything for. A gate that passes when its own evidence is absent is
// not a gate.
func TestCheckRenderDigestRefusesAnUnrecordedRender(t *testing.T) {
	got := CheckRenderDigest(renderUnit, renderHead, renderKey, "aaaa", "", false)
	if len(got) != 1 {
		t.Fatalf("problems = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "nobody reviewed") || !strings.Contains(got[0], renderKey) {
		t.Errorf("problem = %q, want it to say nobody reviewed it and name the key", got[0])
	}
}

// TestCheckRenderDigestTellsAbsentFromMismatched is the render-side version
// of the bug internal/parity found in CheckPlanDigest on 2026-09-08: a
// recorded-but-empty digest fell through to the mismatch branch and printed
// "approved , ours <digest>", describing a race that never happened. Both
// outcomes refuse, so nothing was unsafe; what was wrong was telling the
// operator the wrong story about why.
func TestCheckRenderDigestTellsAbsentFromMismatched(t *testing.T) {
	got := CheckRenderDigest(renderUnit, renderHead, renderKey, "aaaa", "", true)
	if len(got) != 1 {
		t.Fatalf("problems = %v, want exactly one", got)
	}
	if !strings.Contains(got[0], "nobody reviewed") {
		t.Errorf("problem = %q, want the unreviewed message, not a mismatch", got[0])
	}
	if strings.Contains(got[0], "does not match") {
		t.Errorf("problem = %q, want it not to describe a mismatch against an empty digest", got[0])
	}
}

// TestCheckRenderDigestDoesNotBlameTheWorld pins the diagnosis, not just the
// refusal. A plan depends on the tree AND the live infrastructure, so two
// disagreeing plans mean something moved underneath. A render depends on the
// tree alone -- offline, no providers -- so the world cannot be the cause,
// and saying so would send an operator to look at their infrastructure for a
// fault that is in their repository.
func TestCheckRenderDigestDoesNotBlameTheWorld(t *testing.T) {
	got := CheckRenderDigest(renderUnit, renderHead, renderKey, "aaaa", "bbbb", true)
	if len(got) != 1 {
		t.Fatalf("problems = %v, want exactly one", got)
	}
	if strings.Contains(got[0], "the world moved") {
		t.Errorf("problem = %q, want a render-specific cause, not the plan gate's wording", got[0])
	}
	if !strings.Contains(got[0], "the tree and the render disagree") {
		t.Errorf("problem = %q, want it to name the tree and the render", got[0])
	}
	for _, want := range []string{"aaaa", "bbbb"} {
		if !strings.Contains(got[0], want) {
			t.Errorf("problem = %q, want it to print digest %s", got[0], want)
		}
	}
}

// TestCheckRenderDigestExemptsNothing is the rule this gate exists to keep.
//
// CheckPlanDigest exempts exactly one root, "credentials", because CI cannot
// plan it -- reading that root's state means reading the tokens. That is a
// fact about the world, and docs/port-plan.md already calls it the single
// hole in "every apply is gated". Rendering has no equivalent fact: it needs
// no state, no credentials and no network, so a unit CI could not render is
// one the applier cannot render either. Any name that got a free pass here
// would turn one hole into as many as there are kinds.
func TestCheckRenderDigestExemptsNothing(t *testing.T) {
	names := []string{
		"credentials",
		"platform",
		"baselines/prod",
		"deliveries/beta/web",
		"",
	}
	for _, name := range names {
		if got := CheckRenderDigest(name, renderHead, renderKey, "aaaa", "", false); len(got) == 0 {
			t.Errorf("CheckRenderDigest(%q, ...) returned no problems: no unit is exempt from this gate", name)
		}
	}
}
