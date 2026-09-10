---
name: commit
description: Commit in this repository. Use before every commit — it runs the full check chain and carries the message conventions the leak scanner and the reader both enforce.
---

# Committing

## Run the checks first, as their own command

    scripts/check

⚠️ **Never chain it to the commit.** `scripts/check && git commit` commits
either way under some shells' error handling, and it reads a different tree
than it tested. Run it, read the output, then commit.

If a step cannot run in this environment, say so in the report rather than
skipping quietly. A check that did not run is not a check that passed.

## The message

Imperative, and it says why — the reason is the part a reader cannot
reconstruct from the diff:

    Let a store URI name a variable, since that is what an operator runs
    Stop trusting a cached test result, and look at the standard library
    Refuse to require a check the target repository has never reported

Subject alone is enough for a small change. A body earns its place by carrying
evidence: the measurement that set a value, the failure that caused the fix.

## No AI attribution

No `Co-Authored-By` trailer naming an assistant, and no generated-with footer.
`scripts/leakscan` refuses them across all history — not only the commit hook,
because a hook lives on one machine and the scan runs for everybody.

## Branch, don't push to main

Commit on a branch. This repository's protection requires a passing `check`
job, a current branch, and dismisses stale approvals; `scripts/repo-protection`
is what states that, and it is a different payload from `scripts/protection`,
which is what the applier demands of a repository it *manages*.
