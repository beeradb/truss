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
// says which is which: two of the entries below say FINDING, because they
// are real defects nobody has decided to accept, not because the corpus
// needs them to pass -- every one of them is reported as a defect rather
// than an approval.
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

// exitStatusSuffix accepts a reason that is the bash's sentence on one side
// and EXACTLY that sentence plus ": exit status <N>" on the other -- nothing
// more.
//
// ⚠️ IT IS DELIBERATELY THIS STRICT, AND ITS PREDECESSOR WAS NOT. The
// earlier matcher forgave the bash's sentence followed by anything at all
// containing "exit status", which is what a transcript looks like -- so it
// would have gone on passing after tofu's combined output came back. This
// one reconstructs the accepted string and compares it, so the only
// difference it can ever forgive is the exit status itself.
func exitStatusSuffix(d Diff) bool {
	rest, ok := strings.CutPrefix(d.Truss, d.Bash+": exit status ")
	if !ok || rest == "" {
		return false
	}
	if _, err := strconv.Atoi(rest); err != nil {
		return false
	}
	return true
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

// alertReasonIsTheExitStatus accepts an alert whose ONLY difference is that
// truss's failure sentence ends ": exit status <N>" where the bash's ends at
// the sentence. Everything else -- the whole head, the trailing
// "(applied=N noop=M)", any EXPIRING clause -- must be identical.
//
// ⚠️ IT RECONSTRUCTS THE ACCEPTED STRING RATHER THAN PATTERN-MATCHING IT,
// and the version this replaces did not. That one asked whether truss's
// alert began with the bash's head plus ": tofu " and contained "exit
// status" somewhere -- which is also true of the head followed by a
// transcript, so it would have gone on passing after tofu's combined output
// came back. Building the one string that is allowed and comparing it means
// the only thing forgivable is the exit status itself.
func alertReasonIsTheExitStatus(d Diff) bool {
	if d.Kind != "alert" {
		return false
	}
	cut := strings.Index(d.Bash, " (applied=")
	if cut < 0 {
		return false
	}
	head, tail := d.Bash[:cut], d.Bash[cut:]
	rest, ok := strings.CutPrefix(d.Truss, head+": exit status ")
	if !ok {
		return false
	}
	status, ok := strings.CutSuffix(rest, tail)
	if !ok || status == "" {
		return false
	}
	_, err := strconv.Atoi(status)
	return err == nil
}

// Divergences is the whole list. Read the type doc before adding to it.
//
// Re-recorded 2026-09-08 against origin/main's 1,114-line apply.sh, the
// same version docs/port-plan.md cites. Line numbers below are against
// that file, but a file somebody else is still editing rots regardless;
// each entry therefore also names the FUNCTION or the exact code it
// refers to, which is what to search for if a number has moved again.
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
		ID:     "REASON-NAMES-THE-EXIT-STATUS",
		Status: StatusIntended,
		Scenarios: []string{
			"test_tofu_plan_failure_is_ledgered_and_nothing_is_applied",
			"test_tofu_apply_failure_stops_the_pass_and_leaves_later_commit_unapplied",
			"test_rotation_failure_is_ledgered_alerted_and_fails_the_run",
		},
		Accept: exitStatusAnywhere,
		Bash:   "\"tofu apply failed for credentials\"",
		Truss:  "the same sentence plus \": exit status 1\", and nothing else",
		Why: "⚠️ THIS ENTRY REPLACES A SECURITY FINDING, AND THE FINDING WAS REAL. It used to read " +
			"REASON-CARRIES-TOFU-TRANSCRIPT: plan.Runner embedded tofu's combined output in the error and the " +
			"pass put that straight into the reason, which lands in a ledger object anyone holding the bucket " +
			"credential can read AND in a Telegram chat. A provider makes no promise about what it prints in an " +
			"error -- a request body, a resource attribute, a token -- and TrimReason's 800-byte cap yields 800 " +
			"bytes of provider output rather than none. apply.sh:335-345 records the same reasoning under its own " +
			"\"Security review, 2026-09-07\"; the port reintroduced exactly what that review removed, and this " +
			"harness printed it as a diff on 2026-09-08. Fixed: the transcript and the pod's absolute working " +
			"directory are gone, and the full output still reaches the pod log. What remains is the bare exit " +
			"status, kept ON PURPOSE because it separates a tofu that ran and refused from one that could not be " +
			"executed at all -- the bash reports both identically.",
		Ref: "applier/apply.sh:335-345; internal/plan/runner.go wrapExecError; " +
			"internal/plan/runner_test.go TestAFailureReasonIsNotATranscript",
	},
	{
		ID:     "EXPIRY-SWEEP-CANNOT-FAIL-LIKE-1PASSWORD",
		Status: StatusIntended,
		Scenarios: []string{
			"test_a_spent_allowance_fails_the_daily_sweep_and_still_reports",
		},
		Accept: expirySweepCannotFailLike1Password,
		Bash:   "the daily pass fails outright (exit 1) when it cannot list the 1Password `platform` vault -- rate-limited or otherwise unreachable",
		Truss:  "the daily pass succeeds; nothing it reads can be rate-limited",
		Why: "The same cause as EXPIRY-ONE-VAULT-MOUNT, seen at its widest: truss's expiry sweep reads a local " +
			"Vault KV mount, never 1Password, so a fixture that simulates 1Password's rate limit or an unreadable " +
			"vault has no analogue on the truss side -- there is no network call to fail. Decision 4 took `op` out " +
			"of the image entirely, and 'the sweep can no longer be rate-limited by a store it never asks' is the " +
			"direct, unavoidable consequence of that, not a new decision. Recorded separately from " +
			"EXPIRY-ONE-VAULT-MOUNT because the blast radius here is the whole pass -- exit code and alert, not " +
			"just the EXPIRING clause -- which that entry's matcher does not and should not forgive.",
		Ref: "docs/port-plan.md §7 decision 4 and §4.7; same gap as EXPIRY-ONE-VAULT-MOUNT, a different scenario",
	},

	// -----------------------------------------------------------------
	// FINDINGS -- nobody decided these. Reported to the owner as defects.
	// -----------------------------------------------------------------
	{
		ID:     "SECRETS-WORDING-VAULT-VS-MOUNTED",
		Status: StatusFinding,
		Scenarios: []string{
			"test_a_skipped_drift_check_says_so_rather_than_looking_clean",
			"test_infra_admin_token_is_read_only_when_a_project_root_needs_it#2",
		},
		Accept: cfInfraAdminVaultWording,
		Bash:   "\"cf-infra-admin is not mounted\" / \"...is not mounted yet\"",
		Truss:  "\"cf-infra-admin is not in the platform vault\" / \"...is not in the vault yet\"",
		Why: "PORT LAG, TRACED TO THE EXACT COMMIT. The pre-Vault apply.sh (the 851-line file the old corpus was " +
			"recorded from, and the one this wording was ported against) said \"is not in the platform vault\" / " +
			"\"is not in the vault yet\" -- truss's cmd/truss/apply_cmd.go:721 and :880 match it verbatim. Commit " +
			"fff78e1 (\"Vault for the platform's credentials\", #5) renamed the wording to \"is not mounted\" / " +
			"\"is not mounted yet\" as part of the move to file-rendered credentials, and nothing updated truss to " +
			"follow. internal/secrets/dir.go's own Field error already uses the NEW words -- \"is not mounted -- " +
			"is the credential mirror applied and syncing?\" -- written after the same commit, which is why truss " +
			"is internally inconsistent: one call site tracks the bash, these two are frozen at the commit before it.",
		Ref: "cmd/truss/apply_cmd.go:721 and :880 vs internal/secrets/dir.go:41; " +
			"applier/apply.sh:727 and :1044 (was :544 and :805 before fff78e1)",
	},
	{
		ID:     "BASH-DIES-SILENT-BEFORE-HEARTBEAT",
		Status: StatusFinding,
		Scenarios: []string{
			"test_a_pass_with_work_still_needs_the_applying_credentials",
		},
		Accept: silentDeathBeforeHeartbeat,
		Bash:   "exits 1 with NO alert and NO heartbeat/failed record at all -- die() exits before write_heartbeat and send_telegram, the last two lines of the script",
		Truss:  "exits 1 (exit code agrees) but also sends an alert and writes failed/<sha> and heartbeat/applier.json",
		Why: "Nobody decided this either. apply.sh's own comment on write_heartbeat says heartbeat and alert run " +
			"'ALWAYS, at the very end -- a job that speaks only on failure cannot be told apart from one that is " +
			"no longer running' -- but a `die()` for a credential that failed to mount (line 136) happens long " +
			"before those two calls, so the bash breaks its own promise silently in exactly the case that promise " +
			"exists for. truss keeps the promise: buildBaseEnv's error still reaches runApplyPass's failure path, " +
			"which always writes a heartbeat. Reported rather than matched away because it is truss disagreeing " +
			"with what the bash actually does, even though truss is plainly the more correct of the two -- the " +
			"owner should decide whether the bash gets fixed to match, or this becomes an accepted improvement.",
		Ref: "applier/apply.sh:136 (die) vs :1112-1113 (write_heartbeat; send_telegram, unconditionally last)",
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
func exitStatusAnywhere(d Diff) bool {
	if d.Kind == "alert" {
		return alertReasonIsTheExitStatus(d)
	}
	if d.Kind != "value" {
		return false
	}
	return exitStatusSuffix(d)
}

// expirySweepCannotFailLike1Password matches EXPIRY-SWEEP-CANNOT-FAIL-LIKE-
// 1PASSWORD's one shape: the bash fails the whole daily pass because it
// could not list the 1Password `platform` vault, and truss -- which never
// asks 1Password anything -- has nothing to fail on and reports a clean,
// empty pass instead. All three sinks must show exactly that shape, or the
// diff is not this one.
func expirySweepCannotFailLike1Password(d Diff) bool {
	const bashClause = "the expiry sweep could not list the 'platform' vault"
	switch d.Kind {
	case "exit":
		return d.Bash == "1" && d.Truss == "0"
	case "alert":
		return strings.Contains(d.Bash, bashClause) && d.Truss == "platform applier: nothing to apply"
	case "value":
		return d.Key == "heartbeat/applier.json" && d.Path == "/failure" &&
			strings.Contains(d.Bash, bashClause) && d.Truss == "null"
	default:
		return false
	}
}

// cfInfraAdminVaultWording is SECRETS-WORDING-VAULT-VS-MOUNTED's matcher:
// the bash's "is not mounted" / "is not mounted yet" against truss's
// "is not in the platform vault" / "is not in the vault yet", for the
// specific sentences the two known call sites produce. Each pair is pinned
// exactly, on both sides, whether the diff arrives as a bare value or
// embedded in the alert's own frame.
var cfInfraAdminWordingPairs = [][2]string{
	{
		"cf-infra-admin is not mounted: it is minted by credentials/, so that root must be applied before projects/recipes can be",
		"cf-infra-admin is not in the platform vault: it is minted by credentials/, so that root must be applied before projects/recipes can be",
	},
	{
		"cf-infra-admin is not mounted yet",
		"cf-infra-admin is not in the vault yet",
	},
}

func cfInfraAdminVaultWording(d Diff) bool {
	for _, pair := range cfInfraAdminWordingPairs {
		bash, truss := pair[0], pair[1]
		if exact(bash, truss)(d) {
			return true
		}
		if d.Kind == "alert" && strings.Contains(d.Bash, bash) && strings.Contains(d.Truss, truss) &&
			strings.Replace(d.Bash, bash, truss, 1) == d.Truss {
			return true
		}
	}
	return false
}

// silentDeathBeforeHeartbeat matches BASH-DIES-SILENT-BEFORE-HEARTBEAT's one
// shape: the bash produced nothing at all in the sinks a failure normally
// reaches (an alert that never sent, a failed/<sha> record that was never
// written, a heartbeat that was never written), because `die()` exited
// before write_heartbeat and send_telegram ever ran. truss's side must be a
// real, well-formed failure naming the same underlying cause -- a
// credential that failed to mount -- or this does not match.
func silentDeathBeforeHeartbeat(d Diff) bool {
	const cause = "is not mounted -- is the credential mirror applied and syncing?"
	switch d.Kind {
	case "alert":
		return d.Bash == "" && strings.Contains(d.Truss, cause)
	case "extra-key":
		return (d.Key == "failed/sha1" || d.Key == "heartbeat/applier.json") && strings.Contains(d.Truss, cause)
	default:
		return false
	}
}
