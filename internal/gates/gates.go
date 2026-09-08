// Package gates decides whether a commit may be applied.
//
// Every refusal in the system lives here, and nothing here performs I/O: each
// gate is a pure function over state somebody else fetched. That is the whole
// point of the package. In the shell version these decisions were interleaved
// with the `gh` calls that fed them, so the only way to test "an approval on
// an earlier push does not count" was to drive the entire script against stub
// binaries and read a ledger object afterwards. Here it is a function call.
package gates

import (
	"fmt"
	"time"
)

// Protection is main's branch protection, as the forge reports it.
//
// ⚠️ Pointers, not bools. A missing key and an explicit false are different
// facts about a repository, and treating them alike is how this went wrong in
// the shell version: `jq '.allow_force_pushes.enabled // true'` replaced a
// COMPLIANT false with a non-compliant true, because `//` fires on false as
// readily as on null. Absent must be its own case and must never read as
// compliant.
type Protection struct {
	RequiredApprovals     *int
	RequireCodeOwners     *bool
	DismissStaleReviews   *bool
	EnforceAdmins         *bool
	AllowForcePushes      *bool
	RequireUpToDateBranch *bool // required_status_checks.strict
	StatusChecks          []string
}

// ProtectionBar is what the applier refuses to run without. Each field of
// Protection above is checked; adding a field here without checking it is the
// failure this type exists to make visible.
func CheckProtection(p Protection, requiredCheck string) []string {
	var problems []string

	if p.RequiredApprovals == nil || *p.RequiredApprovals < 1 {
		problems = append(problems, "required_approving_review_count is below 1")
	}
	if !isTrue(p.RequireCodeOwners) {
		problems = append(problems, "require_code_owner_reviews is off")
	}
	if !isTrue(p.DismissStaleReviews) {
		problems = append(problems, "dismiss_stale_reviews is off")
	}
	if !isTrue(p.EnforceAdmins) {
		problems = append(problems, "enforce_admins is off")
	}
	// The only one whose COMPLIANT value is false, which is exactly why it
	// needs the explicit nil case rather than a truthiness test.
	if p.AllowForcePushes == nil || *p.AllowForcePushes {
		problems = append(problems, "allow_force_pushes is on, or unreadable")
	}
	if !isTrue(p.RequireUpToDateBranch) {
		problems = append(problems, "required_status_checks.strict is off: an approval against a stale main can merge")
	}
	if !contains(p.StatusChecks, requiredCheck) {
		problems = append(problems, fmt.Sprintf("required status check %q is not required", requiredCheck))
	}
	return problems
}

// PullRequest is the merged PR a commit came from, from the DETAIL endpoint.
//
// ⚠️ Merged must come from `repos/{o}/{r}/pulls/{n}`, never from
// `commits/{sha}/pulls`. The list endpoint returns PR objects with no `merged`
// field at all -- only `merged_at` -- so reading `.merged` there is null for
// every PR ever merged. That gate refused the platform's own first merge.
type PullRequest struct {
	Number         int
	Merged         bool
	MergeCommitSHA string
	HeadSHA        string
}

// Review is one review on that PR.
type Review struct {
	User        string
	State       string // APPROVED, CHANGES_REQUESTED, DISMISSED, COMMENTED
	CommitID    string
	SubmittedAt time.Time
}

// Approval decides whether `sha` was approved by `approver`.
//
// ⚠️ The approval must be AT THE HEAD SHA. An approval carried over from an
// earlier push is an approval of code nobody read; GitHub's own
// dismiss_stale_reviews is asked for in Protection, and this checks it again
// rather than trusting that it was on.
//
// A review by the approver that is NOT at the head sha is ordinary API
// noise, not evidence of anything wrong: dismiss_stale_reviews already
// dismisses it on push, and this gate only cares whether a valid approval
// exists at the head. It is silently skipped rather than reported, so a PR
// that got a second push is judged on its current approval, not wedged by
// its history.
func CheckApproval(pr PullRequest, reviews []Review, approver, sha string) []string {
	var problems []string

	if !pr.Merged {
		problems = append(problems, fmt.Sprintf("PR #%d is not merged", pr.Number))
	}
	// ⚠️ Absent must be its own case, same as everywhere else in this file:
	// an empty MergeCommitSHA is not "no opinion", it is "unmerged, or
	// unreadable", and apply.sh:529 refuses it unconditionally.
	if pr.MergeCommitSHA == "" {
		problems = append(problems, fmt.Sprintf("PR #%d has no merge commit recorded", pr.Number))
	} else if pr.MergeCommitSHA != sha {
		problems = append(problems,
			fmt.Sprintf("PR #%d's merge commit is %s, not %s", pr.Number, short(pr.MergeCommitSHA), short(sha)))
	}

	approved := false
	for _, r := range reviews {
		if r.State != "APPROVED" || r.User != approver {
			continue
		}
		if r.CommitID != pr.HeadSHA {
			continue
		}
		approved = true
	}
	if !approved {
		problems = append(problems,
			fmt.Sprintf("no approval by %s at the PR head %s", approver, short(pr.HeadSHA)))
	}
	return problems
}

// Commit is the merge commit itself, from the forge's commit-detail
// endpoint.
type Commit struct {
	SHA            string
	Verified       *bool
	CommitterLogin *string
}

// CheckMergeCommit refuses a merge commit that the forge did not create
// itself. "Approved by a human, applied by us" only means something if the
// tree at sha is the tree the PR's approval covered, and the only party who
// can vouch for that is GitHub performing its own merge (web-flow) with a
// verified signature (545-556).
//
// ⚠️ Same absent-is-not-compliant shape as Protection and PullRequest: a
// commit whose verification status or committer could not be read is
// refused, never defaulted to true.
func CheckMergeCommit(c Commit) []string {
	var problems []string
	if !isTrue(c.Verified) {
		problems = append(problems, fmt.Sprintf("merge commit %s is not verified", short(c.SHA)))
	}
	committer := "absent"
	if c.CommitterLogin != nil {
		committer = *c.CommitterLogin
	}
	if committer != "web-flow" {
		problems = append(problems,
			fmt.Sprintf("merge commit %s was committed by %s, not github's own web-flow merge -- a merge github did not perform itself is not trustworthy as \"what the approved PR contained\"", short(c.SHA), committer))
	}
	return problems
}

// CheckPlanDigest refuses to apply a plan whose digest does not match the
// one recorded as approved at headSHA -- "mine" is the digest of the plan
// about to run, "approved" is what was recorded when the PR was reviewed,
// and approvedFound distinguishes "recorded and empty" (impossible in
// practice, but not this function's business to assume) from "nothing was
// ever recorded".
//
// The credentials root is the one exemption (631, and §2.10): CI never
// plans it, so there is never anything to compare against.
func CheckPlanDigest(root, headSHA, mine string, approved string, approvedFound bool) []string {
	if root == "credentials" {
		return nil
	}
	var problems []string
	if !approvedFound {
		problems = append(problems,
			fmt.Sprintf("no approved plan recorded for %s at %s: refusing to apply a plan nobody reviewed", root, short(headSHA)))
		return problems
	}
	if mine != approved {
		problems = append(problems,
			fmt.Sprintf("the plan for %s does not match the one approved at %s (approved %s, ours %s): the world moved between review and apply", root, short(headSHA), approved, mine))
	}
	return problems
}

func isTrue(b *bool) bool { return b != nil && *b }

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
