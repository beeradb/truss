package forge

import (
	"context"
	"net/url"

	"github.com/beeradb/truss/internal/gates"
)

// wireProtection mirrors GitHub's branch-protection payload, pointers all
// the way down. A key entirely missing from the JSON leaves every field
// beneath it nil, which is what lets Protection distinguish "not set" from
// "set to false" -- the same distinction gates.Protection's own doc comment
// insists on.
type wireProtection struct {
	RequiredPullRequestReviews *struct {
		RequiredApprovingReviewCount *int  `json:"required_approving_review_count"`
		RequireCodeOwnerReviews      *bool `json:"require_code_owner_reviews"`
		DismissStaleReviews          *bool `json:"dismiss_stale_reviews"`
		RequireLastPushApproval      *bool `json:"require_last_push_approval"`
		// ⚠️ A POINTER TO A STRUCT, NOT A STRUCT. GitHub omits this key
		// entirely when no bypass is configured -- there is no `{}` on the
		// wire for "nobody" -- so BypassPullRequestAllowances stays nil
		// through decode exactly when it should, and gates.Protection reads
		// that nil as compliant. A non-pointer struct would decode to its
		// zero value either way and lose the distinction.
		BypassPullRequestAllowances *struct {
			Users []string `json:"users"`
			Teams []string `json:"teams"`
			Apps  []string `json:"apps"`
		} `json:"bypass_pull_request_allowances"`
	} `json:"required_pull_request_reviews"`
	EnforceAdmins *struct {
		Enabled *bool `json:"enabled"`
	} `json:"enforce_admins"`
	AllowForcePushes *struct {
		Enabled *bool `json:"enabled"`
	} `json:"allow_force_pushes"`
	AllowDeletions *struct {
		Enabled *bool `json:"enabled"`
	} `json:"allow_deletions"`
	RequiredStatusChecks *struct {
		Strict   *bool    `json:"strict"`
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	} `json:"required_status_checks"`
}

func (w wireProtection) toGates() gates.Protection {
	var p gates.Protection

	if w.RequiredPullRequestReviews != nil {
		p.RequiredApprovals = w.RequiredPullRequestReviews.RequiredApprovingReviewCount
		p.RequireCodeOwners = w.RequiredPullRequestReviews.RequireCodeOwnerReviews
		p.DismissStaleReviews = w.RequiredPullRequestReviews.DismissStaleReviews
		p.RequireLastPushApproval = w.RequiredPullRequestReviews.RequireLastPushApproval
		if bpa := w.RequiredPullRequestReviews.BypassPullRequestAllowances; bpa != nil {
			p.BypassPullRequestAllowances = &gates.BypassAllowances{
				Users: bpa.Users,
				Teams: bpa.Teams,
				Apps:  bpa.Apps,
			}
		}
	}
	if w.EnforceAdmins != nil {
		p.EnforceAdmins = w.EnforceAdmins.Enabled
	}
	if w.AllowForcePushes != nil {
		p.AllowForcePushes = w.AllowForcePushes.Enabled
	}
	if w.AllowDeletions != nil {
		p.AllowDeletions = w.AllowDeletions.Enabled
	}
	// Both shapes GitHub has used for the required check list are accepted
	// and merged into one slice: a plain list of context strings, or a list
	// of {context, app_id} objects. gates.CheckProtection only ever asks
	// "is my check's name in this list", so which shape it came from does
	// not need to survive past this decode.
	if w.RequiredStatusChecks != nil {
		p.RequireUpToDateBranch = w.RequiredStatusChecks.Strict
		p.StatusChecks = append(p.StatusChecks, w.RequiredStatusChecks.Contexts...)
		for _, ck := range w.RequiredStatusChecks.Checks {
			p.StatusChecks = append(p.StatusChecks, ck.Context)
		}
	}
	return p
}

// Protection reads main's (or branch's) protection settings. A payload that
// cannot be read at all -- a non-2xx, a malformed body -- is an error, never
// a zero-valued Protection that CheckProtection would then refuse for the
// wrong reason.
func (c *Client) Protection(ctx context.Context, branch string) (gates.Protection, error) {
	path := c.repoPath("/branches/%s/protection", url.PathEscape(branch))
	var w wireProtection
	if err := c.request(ctx, "GET", path, &w); err != nil {
		return gates.Protection{}, err
	}
	return w.toGates(), nil
}
