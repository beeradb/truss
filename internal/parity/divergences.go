package parity

import (
	"regexp"
	"strconv"
	"strings"
)

// Divergence is one place truss does not reproduce the bash, stated so a
// reader can check the claim rather than trust it: the scenario and field
// it shows up in, what each side produces, WHY, and where that was decided.
//
// ⚠️ THIS LIST IS THE DELIVERABLE, NOT A CONVENIENCE. A whole-pass parity
// test over two implementations that genuinely differ has two failure
// modes, and they are opposite: it fails forever, or it gets quietly
// weakened until it passes -- and a parity test weakened to pass is worse
// than none, because it reports success. The list is the third option:
// every difference is written down with an argument, and anything not on it
// fails.
//
// ⚠️ DO NOT ADD AN ENTRY TO MAKE A TEST PASS. An entry claims somebody
// decided this difference on purpose, with a reference that can be read.
// A difference nobody decided is a finding to take to the owner. Status
// says which is which, and five of the entries below say FINDING: they are
// here so the other thirty-two scenarios can be tested at all, and every
// one of them is reported as a defect rather than an approval.
type Divergence struct {
	// ID is stable and is what a failure message names.
	ID string
	// Status is INTENDED for a divergence the port plan or the code
	// argues for, and FINDING for one reported to the owner as a defect.
	Status string
	// Scenarios limits the entry to named scenarios. Empty means every
	// scenario, used only for differences that are structural.
	Scenarios []string
	// Kind, Key and Path narrow the diff this entry accounts for. Empty
	// matches anything.
	Kind, Key, Path string
	// Bash and Truss say, in prose, what each implementation produces.
	Bash, Truss string
	// Why is the argument. Ref names where it was decided, or -- for a
	// FINDING -- where the behaviour it contradicts is written down.
	Why, Ref string
	// Accept narrows further where Kind/Key/Path cannot. nil accepts
	// every diff that reached it.
	Accept func(Diff) bool
}

const (
	// StatusIntended: the port plan, a decision record or a code comment
	// argues for this difference.
	StatusIntended = "INTENDED"
	// StatusFinding: nobody decided this. It is allowed here only so the
	// rest of the corpus can run, and it is reported as a defect.
	StatusFinding = "FINDING"
)

// Matches reports whether this entry accounts for d in scenario.
func (dv Divergence) Matches(scenario string, d Diff) bool {
	if len(dv.Scenarios) > 0 && !inScenarios(dv.Scenarios, scenario) {
		return false
	}
	if dv.Kind != "" && dv.Kind != d.Kind {
		return false
	}
	if dv.Key != "" && dv.Key != d.Key {
		return false
	}
	if dv.Path != "" && dv.Path != d.Path {
		return false
	}
	if dv.Accept != nil && !dv.Accept(d) {
		return false
	}
	return true
}

func inScenarios(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// --- matchers ---------------------------------------------------------------
//
// Every matcher below pins BOTH sides. A matcher that only checked what
// truss produced would accept a future change to the bash's side of the
// same field, which is the hole an allowlist is most likely to grow.

// exact accepts one specific pair of strings and nothing else.
func exact(bash, truss string) func(Diff) bool {
	return func(d Diff) bool { return d.Bash == bash && d.Truss == truss }
}

// tofuTranscript accepts a reason that is the bash's bare sentence on one
// side and the SAME sentence followed by tofu's raw output on the other.
// It pins the bash side exactly and requires truss's to start with it, so
// the only thing it forgives is the appended transcript -- see
// REASON-CARRIES-TOFU-TRANSCRIPT.
func tofuTranscript(bash string) func(Diff) bool {
	return func(d Diff) bool {
		return d.Bash == bash &&
			strings.HasPrefix(d.Truss, bash+": tofu ") &&
			strings.Contains(d.Truss, "exit status ")
	}
}

// runtimeVaultItems are the credentials that live in the second vault the
// bash swept and truss does not. Named here, exhaustively, so the entry
// that forgives their absence cannot forgive the absence of anything else.
var runtimeVaultItems = map[string]bool{
	"cf-images":     true,
	"nyt-cookie":    true,
	"registry-pull": true,
}

// onlyRuntimeVaultItem accepts a heartbeat expiring entry that the bash
// reported and truss did not, but only when the name is one of the three
// above.
func onlyRuntimeVaultItem() func(Diff) bool {
	return func(d Diff) bool {
		if d.Truss != "<absent>" {
			return false
		}
		if strings.HasSuffix(d.Path, "/name") {
			return runtimeVaultItems[d.Bash]
		}
		// days_left for the same absent entry: a number, or null.
		if _, err := strconv.Atoi(d.Bash); err == nil {
			return true
		}
		return d.Bash == "null"
	}
}

var expiringClause = regexp.MustCompile(`; EXPIRING: .*$`)
var expiringEntry = regexp.MustCompile(`^(.*?) (?:in (-?\d+)d|\(no expiry recorded\))$`)

// alertDiffersOnlyInTheExpiringClause is the strongest predicate here and
// the one worth reading. It requires that:
//
//   - everything before "; EXPIRING:" is byte-identical, so a divergence
//     anywhere else in the alert is NOT forgiven by this entry;
//   - every credential truss names, the bash also named;
//   - every credential the bash named and truss did not is one of the
//     three that live in the vault mount truss has no store for;
//   - and where both name one, their day counts differ by at most a day
//     -- the clock tolerance, since the two implementations read `now`
//     seconds apart and both truncate toward zero.
func alertDiffersOnlyInTheExpiringClause(d Diff) bool {
	if d.Kind != "alert" {
		return false
	}
	if expiringClause.ReplaceAllString(d.Bash, "") != expiringClause.ReplaceAllString(d.Truss, "") {
		return false
	}
	bash := parseExpiringClause(d.Bash)
	truss := parseExpiringClause(d.Truss)
	for name := range truss {
		if _, ok := bash[name]; !ok {
			return false
		}
	}
	for name, bDays := range bash {
		tDays, ok := truss[name]
		if !ok {
			if !runtimeVaultItems[name] {
				return false
			}
			continue
		}
		if bDays == nil || tDays == nil {
			if (bDays == nil) != (tDays == nil) {
				return false
			}
			continue
		}
		if *bDays-*tDays > dayCountTolerance || *tDays-*bDays > dayCountTolerance {
			return false
		}
	}
	return true
}

// parseExpiringClause reads "; EXPIRING: a in 3d, b (no expiry recorded)"
// back into a map. A nil value is "no expiry recorded".
func parseExpiringClause(text string) map[string]*int {
	out := map[string]*int{}
	i := strings.Index(text, "; EXPIRING: ")
	if i < 0 {
		return out
	}
	for _, part := range strings.Split(text[i+len("; EXPIRING: "):], ", ") {
		m := expiringEntry.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			continue
		}
		if m[2] == "" {
			out[m[1]] = nil
			continue
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		out[m[1]] = &n
	}
	return out
}

// alertReasonIsTheTranscript accepts an alert whose only difference is that
// truss's failure sentence carries tofu's transcript where the bash's does
// not. Both alerts are otherwise identical, including the trailing
// "(applied=N noop=M)" and any EXPIRING clause.
func alertReasonIsTheTranscript(d Diff) bool {
	if d.Kind != "alert" {
		return false
	}
	// The bash's whole alert must be a prefix-and-suffix of truss's, with
	// only a transcript inserted at the point the reason ends.
	cut := strings.Index(d.Bash, " (applied=")
	if cut < 0 {
		return false
	}
	head, tail := d.Bash[:cut], d.Bash[cut:]
	return strings.HasPrefix(d.Truss, head+": tofu ") &&
		strings.HasSuffix(d.Truss, tail) &&
		strings.Contains(d.Truss, "exit status ")
}

// Divergences is the whole list. Read the type doc before adding to it.
//
// ⚠️ THE apply.sh LINE NUMBERS BELOW ARE AGAINST THE 851-LINE FILE THIS
// CORPUS WAS RECORDED FROM, WHICH IS NOT THE ONE docs/port-plan.md CITES
// (it names a 1,114-line version). Line numbers in a file somebody else is
// still editing rot; each entry therefore also names the FUNCTION or the
// exact code it refers to, which is what to search for if the number has
// moved.
var Divergences = []Divergence{
	// -----------------------------------------------------------------
	// INTENDED
	// -----------------------------------------------------------------
	{
		ID:     "EXPIRY-ONE-VAULT-MOUNT",
		Status: StatusIntended,
		Scenarios: []string{
			"test_hand_credentials_near_expiry_are_named_in_heartbeat_and_alert",
		},
		Kind:   "value",
		Key:    "heartbeat/applier.json",
		Accept: onlyRuntimeVaultItem(),
		Bash:   "sweeps two 1Password vaults, `platform` and `recipes-runtime`, so nyt-cookie is reported",
		Truss:  "sweeps one Vault KV mount, `platform`; nothing in `recipes-runtime` is reported at all",
		Why: "Decision 4 made Vault authoritative for credential lifetimes and took `op` out of the image. " +
			"Only the `platform` mount exists; the runtime vault has no Vault mount yet, and inventing a second " +
			"store here to make the comparison come out even would be exactly the vacuous guard this repo has a " +
			"standing rule against. Sweep.Stores is a slice so the fix is a configuration change, not a code one. " +
			"⚠️ THE GAP IS REAL AND UNCLOSED: until that mount exists, three credentials expire unannounced.",
		Ref: "docs/port-plan.md §7 decision 4 and §4.7 (\"A SECOND GAP, LARGER THAN DECISION 4 STATES\"); " +
			"cmd/truss/expiry_cmd.go's ⚠️ on runExpirySweep",
	},
	{
		ID:     "EXPIRY-ALERT-CLAUSE",
		Status: StatusIntended,
		Scenarios: []string{
			"test_hand_credentials_near_expiry_are_named_in_heartbeat_and_alert",
		},
		Kind:   "alert",
		Accept: alertDiffersOnlyInTheExpiringClause,
		Bash:   "the EXPIRING clause names the runtime vault's credentials too",
		Truss:  "the same clause, minus them; day counts may differ by one",
		Why: "The same single-mount scope as EXPIRY-ONE-VAULT-MOUNT, seen through the alert. The day-count " +
			"tolerance is the clock: the two implementations read `now` seconds apart and both truncate toward " +
			"zero, so a fixture written as \"ten days from now\" legitimately reads 9 in one and 10 in the other. " +
			"The predicate requires everything before \"; EXPIRING:\" to be byte-identical, so this entry cannot " +
			"forgive a divergence anywhere else in the alert.",
		Ref: "docs/port-plan.md §7 decision 4; internal/secrets/sweep.go's DaysUntil, on truncation",
	},
	{
		ID:     "GATE-APPROVAL-WORDING",
		Status: StatusIntended,
		Scenarios: []string{
			"test_approval_on_an_older_head_sha_is_refused",
			"test_rotation_never_advances_past_a_commit_the_loop_refused",
		},
		Accept: exactOrAlert(
			"no APPROVED review by alice at head sha headsha1 for PR #42 (an approval on an earlier push does not count)",
			"no approval by alice at the PR head headsha1"),
		Bash:  "one sentence naming the PR number and explaining that a stale approval does not count",
		Truss: "gates.CheckApproval's own wording, one entry per problem found",
		Why: "internal/gates was specified as pure functions each returning EVERY problem it found, rather than " +
			"one function that formats a sentence, so a refusal can name two faults at once instead of the first. " +
			"That changes the text by construction. Both refuse, at the same point, for the same reason. " +
			"⚠️ WORTH THE OWNER'S EYE ANYWAY: the PR number is gone from the message, and it is what somebody " +
			"reading the alert would open next -- reported alongside this, not approved by it.",
		Ref: "docs/port-plan.md §4.5; internal/gates/gates.go package doc",
	},
	{
		ID:     "GATE-MERGE-COMMIT-WORDING",
		Status: StatusIntended,
		Scenarios: []string{
			"test_merge_commit_not_githubs_own_is_refused",
		},
		Accept: exactOrAlert(
			"merge commit sha1 is not GitHub's own merge (verified=false committer=someone) -- a merge GitHub did not perform itself is not trustworthy as \"what the approved PR contained\"",
			"merge commit sha1 is not verified; merge commit sha1 was committed by someone, not github's own web-flow merge -- a merge github did not perform itself is not trustworthy as \"what the approved PR contained\""),
		Bash:  "one sentence carrying both facts, `verified=false committer=someone`",
		Truss: "two problems, joined with \"; \" -- an unverified signature and a committer that is not web-flow",
		Why: "Same cause as GATE-APPROVAL-WORDING: CheckMergeCommit reports both faults separately rather than " +
			"folding them into one line, so a commit that fails only one of them says which. The refusal, and " +
			"the commit it refuses, are identical.",
		Ref: "internal/gates/gates.go CheckMergeCommit; docs/port-plan.md §4.5",
	},

	// -----------------------------------------------------------------
	// FINDINGS -- nobody decided these. Reported to the owner as defects.
	// -----------------------------------------------------------------
	{
		ID:     "REASON-CARRIES-TOFU-TRANSCRIPT",
		Status: StatusFinding,
		Scenarios: []string{
			"test_tofu_plan_failure_is_ledgered_and_nothing_is_applied",
			"test_tofu_apply_failure_stops_the_pass_and_leaves_later_commit_unapplied",
			"test_rotation_failure_is_ledgered_alerted_and_fails_the_run",
		},
		Accept: transcriptAnywhere,
		Bash:   "\"tofu apply failed for credentials\" -- the sentence and nothing else",
		Truss:  "the same sentence, plus tofu's combined output and the pod's absolute working directory",
		Why: "⚠️ SECURITY. apply.sh:171-180 is explicit that a failure reason is not a transcript, because it " +
			"lands in a ledger object anyone holding the bucket credential can read AND in a Telegram chat, and " +
			"a provider makes no promise about what it prints in an error -- request bodies, resource attributes, " +
			"a token. plan.Runner's wrapExecError embeds the combined output in the error, and the pass puts the " +
			"error straight into the reason. TrimReason caps it at 800 bytes, which is 800 bytes of provider " +
			"output rather than none. Nothing in §3 permits this and the bash's own comment argues against it.",
		Ref: "applier/apply.sh:171-180; docs/port-plan.md §2 item 13; internal/plan/runner.go wrapExecError",
	},
	{
		ID:     "ROTATION-FAILURE-NOT-LEDGERED",
		Status: StatusFinding,
		Scenarios: []string{
			"test_rotation_failure_is_ledgered_alerted_and_fails_the_run",
			"test_tofu_plan_failure_is_ledgered_and_nothing_is_applied",
			"test_tofu_apply_failure_stops_the_pass_and_leaves_later_commit_unapplied",
		},
		Kind:   "missing-key",
		Accept: func(d Diff) bool { return strings.HasSuffix(d.Key, "/rotation-<ts>") },
		Bash:   "files the rotation failure at failed/rotation-<UTC timestamp>, under its own key",
		Truss:  "writes no ledger object for it at all",
		Why: "rotate_credentials calls ledger_put_failed with its own key precisely so a rotation failure is not " +
			"filed against a commit that did not cause it, and so it survives in the ledger after the heartbeat " +
			"has been overwritten by the next pass five minutes later. runRotation returns its error and nothing " +
			"writes it. The heartbeat still carries it, so the loss is the durable record, not the alarm.",
		Ref: "applier/apply.sh:697 (ledger_put_failed \"rotation-…\"); cmd/truss/apply_cmd.go runRotation",
	},
	{
		ID:     "FAILURE-PRECEDENCE-COMMIT-BEFORE-ROTATION",
		Status: StatusFinding,
		Scenarios: []string{
			"test_tofu_plan_failure_is_ledgered_and_nothing_is_applied",
			"test_tofu_apply_failure_stops_the_pass_and_leaves_later_commit_unapplied",
		},
		Accept: firstFailureWins,
		Bash:   "the rotation failure overwrites the commit's, so the alert names rotation and not the commit that failed",
		Truss:  "the commit's failure is kept and rotation's is reported only in the rotation summary",
		Why: "The bash assigns `failure=` unconditionally in rotate_credentials, which clobbers whatever the " +
			"commit loop set; truss guards it with `failure == \"\"`. Truss's behaviour is BETTER -- the commit " +
			"failure is the one somebody has to act on, and the bash's alert hides it -- but it is a behaviour " +
			"change that §3 does not list, and §5.5 asks for equal alert text. Recorded as a finding rather " +
			"than an approval so the owner decides, not this file.",
		Ref: "applier/apply.sh:698 vs cmd/truss/apply_cmd.go's `if rotErr != nil && failure == \"\"`",
	},
	{
		ID:     "DIGEST-GATE-EMPTY-READS-AS-MISMATCH",
		Status: StatusFinding,
		Scenarios: []string{
			"test_a_root_with_no_approved_plan_is_refused",
		},
		Accept: emptyDigestRefusal,
		Bash:   "\"no approved plan recorded for <root> at <sha> (<the key>): refusing to apply a plan nobody reviewed\"",
		Truss:  "\"the plan for <root> does not match the one approved at <sha> (approved , ours <digest>): the world moved between review and apply\"",
		Why: "Two things, both message-only -- BOTH IMPLEMENTATIONS REFUSE, which is the property that matters. " +
			"First, an EMPTY recorded digest is \"nobody reviewed this\" and truss reads it as a mismatch; " +
			"gates.CheckPlanDigest's own comment calls that case \"impossible in practice\", and the reference " +
			"suite has a test for it, so it is not. Second, the bash names the ledger KEY and truss does not -- " +
			"the key is what an operator would go and look at. Nothing in §3 covers either.",
		Ref: "applier/apply.sh:462 (`[ -z \"$theirs\" ]`); internal/gates/gates.go CheckPlanDigest",
	},
}

// exactOrAlert accepts the same pair of strings whether it arrives as a
// ledger field or embedded in the alert's own "FAILED at <sha>: … (applied=…)"
// frame -- one refusal reaches three sinks and they must not need three
// entries that could drift apart.
func exactOrAlert(bash, truss string) func(Diff) bool {
	plain := exact(bash, truss)
	return func(d Diff) bool {
		if plain(d) {
			return true
		}
		if d.Kind != "alert" {
			return false
		}
		return strings.Contains(d.Bash, bash) && strings.Contains(d.Truss, truss) &&
			strings.Replace(d.Bash, bash, truss, 1) == d.Truss
	}
}

// transcriptAnywhere is REASON-CARRIES-TOFU-TRANSCRIPT's matcher across all
// three sinks a reason reaches: failed/<sha>, the heartbeat, and the alert.
func transcriptAnywhere(d Diff) bool {
	if d.Kind == "alert" {
		return alertReasonIsTheTranscript(d)
	}
	if d.Kind != "value" {
		return false
	}
	return tofuTranscript(d.Bash)(d)
}

// firstFailureWins matches the one shape FAILURE-PRECEDENCE names: the
// bash blames rotation where truss blames the commit that actually failed.
//
// Both sides must still BE a failure -- an entry that accepted "one failed
// and the other did not" would forgive the pass going green, which is the
// only thing this scenario is really guarding. In the alert the two
// divergences arrive together (truss's reason also carries the transcript,
// see REASON-CARRIES-TOFU-TRANSCRIPT), so the alert branch checks both
// halves rather than pretending they can be separated.
func firstFailureWins(d Diff) bool {
	switch d.Kind {
	case "alert":
		return strings.Contains(d.Bash, "FAILED at ") &&
			strings.Contains(d.Bash, ": rotation of credentials at ") &&
			strings.Contains(d.Truss, "FAILED at ") &&
			!strings.Contains(d.Truss, "rotation of credentials at ") &&
			strings.Contains(d.Truss, " failed for ") &&
			strings.Contains(d.Truss, "exit status ")
	case "value":
		return strings.HasPrefix(d.Bash, "rotation of credentials at ") &&
			!strings.HasPrefix(d.Truss, "rotation of credentials at ") &&
			strings.Contains(d.Truss, " failed for ")
	default:
		return false
	}
}

// emptyDigestRefusal matches DIGEST-GATE-EMPTY-READS-AS-MISMATCH, and
// requires BOTH sides to be a refusal naming the same root: the difference
// forgiven here is which sentence, never whether one was said.
func emptyDigestRefusal(d Diff) bool {
	return strings.Contains(d.Bash, "refusing to apply a plan nobody reviewed") &&
		strings.Contains(d.Truss, "does not match the one approved") &&
		strings.Contains(d.Truss, "(approved , ours ") &&
		strings.Contains(d.Bash, "projects/recipes") &&
		strings.Contains(d.Truss, "projects/recipes")
}
