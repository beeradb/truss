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
func CheckApproval(pr PullRequest, reviews []Review, approver, sha string) []string {
	var problems []string

	if !pr.Merged {
		problems = append(problems, fmt.Sprintf("PR #%d is not merged", pr.Number))
	}
	if pr.MergeCommitSHA != "" && pr.MergeCommitSHA != sha {
		problems = append(problems,
			fmt.Sprintf("PR #%d's merge commit is %s, not %s", pr.Number, short(pr.MergeCommitSHA), short(sha)))
	}

	approved := false
	for _, r := range reviews {
		if r.State != "APPROVED" || r.User != approver {
			continue
		}
		if r.CommitID != pr.HeadSHA {
			problems = append(problems,
				fmt.Sprintf("approval by %s is at %s, but the PR head is %s: it approved a different tree",
					r.User, short(r.CommitID), short(pr.HeadSHA)))
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
