package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/gates"
)

// cmdGate dispatches `gate protection` and `gate commit <sha>`, replacing
// check_branch_protection and validate_pr + verify_github_merge_commit
// (§4.9). Exit codes: 0 pass, 1 refuse with the reason on stdout, 2
// could-not-ask -- the three must never collapse into two, because a
// compromised or broken checker must not be able to look like either a pass
// or a refusal (§4.9, "Exit-code contract").
func cmdGate(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: truss gate protection | truss gate commit <sha>")
		return 2
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 2
	}
	client, err := buildForgeClient(cfg, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	switch args[0] {
	case "protection":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: truss gate protection")
			return 2
		}
		prot, err := client.Protection(ctx, "main")
		if err != nil {
			fmt.Fprintf(stderr, "gate protection: could not read branch protection for main: %v\n", err)
			return 2
		}
		gateProblems := gates.CheckProtection(prot, cfg.RequiredCheck)
		if len(gateProblems) > 0 {
			fmt.Fprintf(stdout, "branch protection on main does not meet the bar: %s\n", strings.Join(gateProblems, "; "))
			return 1
		}
		return 0

	case "commit":
		if len(args) != 2 {
			fmt.Fprintln(stderr, "usage: truss gate commit <sha>")
			return 2
		}
		sha := args[1]
		headSHA, prNumber, reason, ioErr := checkCommitGate(ctx, client, cfg.Approver, sha)
		if ioErr != nil {
			fmt.Fprintf(stderr, "gate commit: %v\n", ioErr)
			return 2
		}
		if reason != "" {
			fmt.Fprintf(stdout, "FAIL\t%s\n", reason)
			return 1
		}
		fmt.Fprintf(stdout, "OK\t%s\t%d\n", headSHA, prNumber)
		return 0

	default:
		fmt.Fprintln(stderr, "usage: truss gate protection | truss gate commit <sha>")
		return 2
	}
}

// checkCommitGate is validate_pr and verify_github_merge_commit fused into
// one call, shared by `truss gate commit` and the apply pass's own loop.
//
// It returns exactly one of: a non-nil ioErr (a forge call failed -- "could
// not ask"), a non-empty reason (a gate refused the commit), or both empty
// (the commit passes both gates, with headSHA and prNumber for the caller
// to use -- CI planned the PR's HEAD, not the merge commit, and the plan
// digest is filed under that sha).
func checkCommitGate(ctx context.Context, client forgeGateway, approver, sha string) (headSHA string, prNumber int, reason string, ioErr error) {
	numbers, err := client.PullNumbersForCommit(ctx, sha)
	if err != nil {
		return "", 0, "", fmt.Errorf("could not list pull requests for %s: %w", sha, err)
	}
	if len(numbers) != 1 {
		return "", 0, fmt.Sprintf("expected exactly one PR for %s, found %d", sha, len(numbers)), nil
	}
	number := numbers[0]

	pr, err := client.PullRequest(ctx, number)
	if err != nil {
		return "", 0, "", fmt.Errorf("could not read PR #%d: %w", number, err)
	}

	reviews, err := client.Reviews(ctx, number)
	if err != nil {
		return "", 0, "", fmt.Errorf("could not read reviews for PR #%d: %w", number, err)
	}

	if problems := gates.CheckApproval(pr, reviews, approver, sha); len(problems) > 0 {
		return "", 0, strings.Join(problems, "; "), nil
	}

	commit, err := client.Commit(ctx, sha)
	if err != nil {
		return "", 0, "", fmt.Errorf("could not read commit %s: %w", sha, err)
	}
	if problems := gates.CheckMergeCommit(commit); len(problems) > 0 {
		return "", 0, strings.Join(problems, "; "), nil
	}

	return pr.HeadSHA, pr.Number, "", nil
}
