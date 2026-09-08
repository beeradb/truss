package forge

import (
	"context"
	"net/url"

	"github.com/beeradb/truss/internal/gates"
)

// wireCommitDetail mirrors `commits/{sha}`. Pointers again, all the way to
// the leaves: `committer` itself can be a JSON null (GitHub could not match
// the commit to an account) and so can its `login`, and both must decode to
// nil rather than to an empty string that would print as "absent" by
// accident instead of by fact.
type wireCommitDetail struct {
	SHA    string `json:"sha"`
	Commit *struct {
		Verification *struct {
			Verified *bool `json:"verified"`
		} `json:"verification"`
	} `json:"commit"`
	Committer *struct {
		Login *string `json:"login"`
	} `json:"committer"`
}

func (w wireCommitDetail) toGates() gates.Commit {
	c := gates.Commit{SHA: w.SHA}
	if w.Commit != nil && w.Commit.Verification != nil {
		c.Verified = w.Commit.Verification.Verified
	}
	if w.Committer != nil {
		c.CommitterLogin = w.Committer.Login
	}
	return c
}

// Commit reads one commit's detail -- the endpoint verify_github_merge_commit
// (apply.sh:545) reads `.commit.verification.verified` and `.committer.login`
// from.
func (c *Client) Commit(ctx context.Context, sha string) (gates.Commit, error) {
	path := c.repoPath("/commits/%s", url.PathEscape(sha))
	var w wireCommitDetail
	if err := c.request(ctx, "GET", path, &w); err != nil {
		return gates.Commit{}, err
	}
	return w.toGates(), nil
}
