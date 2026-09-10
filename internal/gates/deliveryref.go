package gates

import (
	"fmt"
	"strings"
)

// Rule types this gate requires, in GitHub's own vocabulary rather than a
// normalised one. Read from the live API's published schema on 2026-09-10;
// the effective-rules endpoint reports the rules that apply to one ref, which
// is the question a gate about one ref wants asked.
const (
	ruleNonFastForward = "non_fast_forward"
	ruleDeletion       = "deletion"
)

// CheckDeliveryRef refuses to publish a commit onto the ref a reconciler
// tracks unless that ref's history is append-only.
//
// The applier advances this ref only after it has gated a commit and matched
// every render against the digest CI filed. That makes the ref the boundary
// between "reviewed" and "running", and the property it has to keep is the
// one main keeps: history is the audit log. A force push would rewrite what
// a cluster was told to apply; a deletion would erase it.
//
// ⚠️ WHAT THIS DOES NOT PROVE, STATED SO NOBODY READS MORE INTO A GREEN PASS.
// It does not prove that only the applier can move the ref. Blocking
// non-fast-forward and deletion leaves an ordinary fast-forward push open to
// anyone with write access -- which is deliberate, because the applier itself
// needs exactly that and needs no bypass actor to do it. Restricting the
// pusher would take an `update` rule whose sole bypass actor is the applier's
// App, and the effect of that combination could not be measured here.
//
// The deployment this was built for accepts that: docs/threat-model.md
// already places the approver's own accounts out of scope, so on a repository
// whose only writers are the approver and the applier, push-exclusivity
// defends against a party the model has already excluded. ⚠️ THAT CEASES TO
// BE TRUE THE MOMENT A SECOND HUMAN OR A CI JOB GETS WRITE ACCESS, and
// nothing here notices when it does. docs/work-items.md carries the entry.
//
// A ref nothing protects is refused rather than published to: an unprotected
// ref is not a weaker gate, it is a path to production nobody is watching.
func CheckDeliveryRef(ref string, rs Rulesets) []string {
	problems := CheckRulesets(rs)

	if len(rs.Applicable) == 0 {
		return append(problems, fmt.Sprintf(
			"no ruleset applies to %s: refusing to publish onto a ref whose history nothing protects -- "+
				"create one targeting this ref with the %q and %q rules", ref, ruleNonFastForward, ruleDeletion))
	}

	// The union across every applicable ruleset, because two rulesets may
	// each contribute one of the rules and between them protect the ref.
	// Asking each ruleset to carry both would refuse a correct configuration.
	have := map[string]bool{}
	for _, r := range rs.Applicable {
		for _, t := range r.Rules {
			have[t] = true
		}
	}

	// Built from a fixed-order slice, so the message is deterministic without
	// sorting -- internal/gates may not import sort, and does not need to.
	var missing []string
	for _, want := range []string{ruleNonFastForward, ruleDeletion} {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf(
			"the rulesets applying to %s do not include %s: history on this ref is what a cluster was told to apply, "+
				"so it must be append-only -- add %s to a ruleset targeting it",
			ref, strings.Join(missing, " and "), strings.Join(missing, " and ")))
	}
	return problems
}
