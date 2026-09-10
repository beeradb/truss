package gates

import (
	"fmt"
	"strings"
)

// AnsibleTargets is what the applier believes about one play's hosts,
// gathered from the committed inventory (Declared) and from whichever
// provider of live host evidence vouches for each host (Unknown,
// Unreachable).
//
// ⚠️ IT NAMES NO VENDOR AND MUST NOT START. Tailscale is one source of
// these three lists and was for a while the only one, which is a fact about
// the caller and never about this gate: what refuses a play is the shape of
// its target set, so a deployment reaching its machines some other way is
// judged by the identical rules. The caller's own log says which provider
// produced each fact, and -- crucially -- whether any provider in the pass
// was unable to produce Unknown at all, which an empty Unknown here cannot
// distinguish from a clean fleet. It is the ansible half of the digest gate's job, done a
// different way: KindAnsible has no plan digest at all, because CI cannot
// reach the hosts a play would run against (internal/repo/units.go,
// KindAnsible's own doc comment), so what review means for a play is the
// diff itself, the precedent docs/credentials.md states for the credentials
// root -- plus this check, which a digest could never stand in for because
// no digest speaks to whether the hosts named in the diff still exist.
type AnsibleTargets struct {
	Play string
	// Declared is the hosts the committed inventory says this play
	// configures.
	Declared []string
	// Unknown is every device carrying the managed tag with no inventory
	// record -- the dangerous direction, mirroring
	// tailnet.Findings.UnknownTagged's own doc comment: a machine claiming
	// to be managed that nobody declared is either an intruder or a host
	// somebody forgot, and both need a person.
	Unknown []string
	// Unreachable is every declared host that whichever provider vouches
	// for it could not observe right now -- absent from the tailnet or long
	// unseen, or a stated address that answered nothing.
	Unreachable []string
}

// CheckAnsibleTargets refuses a play whose target set the applier cannot
// vouch for.
//
// ⚠️ A NON-EMPTY Unknown MUST REFUSE EVERY PLAY IN THE PASS, NOT ONLY THIS
// ONE -- and this function cannot enforce that by itself, because it is
// handed one play's targets at a time. The caller that drives every play of
// a pass through this gate must treat ANY play's Unknown as a reason to
// refuse the whole pass: a machine claiming to be managed that nobody
// declared is either an intruder or a host somebody forgot, and continuing
// to configure other hosts while one is unexplained is the wrong instinct,
// the same way a single unexplained device does not become less
// unexplained because the play in front of it does not happen to name it.
// docs/work-items.md records this as unwired and names it as part of what
// wiring must do.
//
// An Unreachable declared host is refused, never skipped: absent is not
// "fine" for a host the inventory says this play configures, the same rule
// tailnet.Findings.Unreachable's own doc comment states. ⚠️ The refusal
// does not say WHERE it was unreachable, because that depends on which
// provider vouches for it; the caller names the provider on the line above. And an empty
// Declared is refused outright -- a play targeting nothing is a play whose
// inventory wiring is broken, and running it with no --limit built from an
// empty set is exactly the shape internal/ansible.Runner refuses on the
// execution side, for the identical reason: nothing declared must never
// silently become "run against everything".
//
// Every offender is reported, not just the first, matching
// CheckDeclarations's own convention: an operator fixing refusals one at a
// time, re-running the whole pass between each, is the outcome this
// function exists to avoid.
func CheckAnsibleTargets(t AnsibleTargets) []string {
	var problems []string
	if len(t.Unknown) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: device(s) carry the managed tag with no inventory record (%s): a machine claiming to be managed that nobody declared is either an intruder or a host somebody forgot, and both need a person -- refusing every play in this pass, not only %s",
			t.Play, strings.Join(t.Unknown, ", "), t.Play))
	}
	if len(t.Unreachable) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: declared host(s) unreachable (%s): absent is not \"fine\"",
			t.Play, strings.Join(t.Unreachable, ", ")))
	}
	if len(t.Declared) == 0 {
		problems = append(problems, fmt.Sprintf(
			"%s: no declared hosts: a play targeting nothing has broken inventory wiring, and running it with no limit would target everything",
			t.Play))
	}
	return problems
}
