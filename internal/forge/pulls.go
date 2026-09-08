package forge

import (
	"context"
	"net/url"
	"time"

	"github.com/beeradb/truss/internal/gates"
)

// wirePRNumber is deliberately the only field this package reads off
// `commits/{sha}/pulls`. That endpoint returns PR objects with no `merged`
// field at all -- only `merged_at` -- so a struct here that could carry a
// Merged bool would silently decode to false for every entry and look like
// evidence rather than the absence it is. See PullNumbersForCommit's return
// type for the other half of this guard: it does not return this type, or
// any type, at all.
type wirePRNumber struct {
	Number int `json:"number"`
}

// PullNumbersForCommit returns the PR numbers associated with sha, and
// nothing else about them. []int is the whole guard: a struct could grow a
// Merged field later without anyone noticing, an int slice cannot.
func (c *Client) PullNumbersForCommit(ctx context.Context, sha string) ([]int, error) {
	path := c.repoPath("/commits/%s/pulls", url.PathEscape(sha))
	var items []wirePRNumber
	if err := c.request(ctx, "GET", path, &items); err != nil {
		return nil, err
	}
	numbers := make([]int, len(items))
	for i, it := range items {
		numbers[i] = it.Number
	}
	return numbers, nil
}

// wirePullRequest is the PR *detail* payload -- `pulls/{number}` -- the only
// endpoint that carries `merged` and an authoritative `merge_commit_sha`.
type wirePullRequest struct {
	Number         int    `json:"number"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// PullRequest reads one PR by number from the detail endpoint. `merged` and
// `merge_commit_sha` never come from anywhere else in this package (see
// PullNumbersForCommit).
func (c *Client) PullRequest(ctx context.Context, number int) (gates.PullRequest, error) {
	path := c.repoPath("/pulls/%d", number)
	var w wirePullRequest
	if err := c.request(ctx, "GET", path, &w); err != nil {
		return gates.PullRequest{}, err
	}
	return gates.PullRequest{
		Number:         w.Number,
		Merged:         w.Merged,
		MergeCommitSHA: w.MergeCommitSHA,
		HeadSHA:        w.Head.SHA,
	}, nil
}

type wireReview struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Reviews returns every review left on a PR, in the order the forge reports
// them. CheckApproval decides which one (if any) counts; this package only
// fetches.
func (c *Client) Reviews(ctx context.Context, number int) ([]gates.Review, error) {
	path := c.repoPath("/pulls/%d/reviews", number)
	var items []wireReview
	if err := c.request(ctx, "GET", path, &items); err != nil {
		return nil, err
	}
	reviews := make([]gates.Review, len(items))
	for i, w := range items {
		reviews[i] = gates.Review{
			User:        w.User.Login,
			State:       w.State,
			CommitID:    w.CommitID,
			SubmittedAt: w.SubmittedAt,
		}
	}
	return reviews, nil
}
