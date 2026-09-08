# Threat model

## What stops each attack

| If someone… | What happens | Enforced by |
| --- | --- | --- |
| opens a PR that edits the plan workflow to exfiltrate its secrets | the workflow that runs is `main`'s; the PR is refused a plan | `pull_request_target` + path refusal |
| adds a malicious provider or a `provisioner "local-exec"` | plan refused | `-plugin-dir` allowlist, grep for provisioner/external |
| gets an agent's forge login | can open PRs. Cannot approve, cannot merge | CODEOWNERS + required code-owner review |
| approves their own PR with any other account | approval ignored | code-owner review required |
| pushes a new commit after the approval | approval dismissed; the applier also checks the sha | dismiss-stale + `pr.head.sha` check |
| turns branch protection off, using the approver's own account | the applier refuses EVERYTHING and alerts | runtime protection check |
| pushes directly to `main` (protection off) | not a forge merge commit → refused | signature/committer check |
| gets the plan job's secrets | reads configuration and state. Changes nothing, reads no token | plan-tier credentials are read-only, and those states hold no secret |
| swaps what would be applied between review and merge — a compromised CI, a resource that moved, a root applied earlier in the same pass | the applier's own plan stops matching the digest CI filed, so the root is refused rather than applied | the plan-digest gate |
| gets root on the box the applier runs on | has everything. **This is the trust root**, stated, not hidden | — |

## What this does NOT protect against

- **Root on the machine the applier runs on** reads its vault credential and
  therefore everything. That node is the trust root. It is why it should run
  the applier and nothing else — a machine that can change everything must not
  also be the machine parsing untrusted input off the open internet.
- **The approver's accounts** are the root of everything above. Both the forge
  and the vault login belong behind hardware keys.
- **Secrets set straight on a provider by hand** — a platform's own
  `secret put` command, for instance — are outside this design. Nothing here
  knows they exist, rotates them, or watches them expire.
- **Branch protection may cost money.** On some plans it cannot be enabled on
  a private repository at all, and the applier then refuses everything —
  correct, but check before the first day.
- **A structurally exceeded rate limit is not fixable by backoff.** If a pass
  reads more from the vault than the account is allowed per hour, retrying
  with backoff only spreads the failure out. Budget the reads, then set the
  cadence from the budget.
- **There's no override for a single stuck gate — only an all-or-nothing one.**
  No gate can be waived, skipped or forced individually: a check refusing for a
  reason that turns out to be wrong cannot be argued with. What exists instead
  is turning protection off entirely, fixing the condition, and turning it back
  on — `scripts/protection off` / `on`, described in
  [docs/operations.md](operations.md).

  That is a real escape hatch and it is deliberately a blunt one. It cannot be
  aimed at one commit or one rule, it takes the whole repository out of
  enforcement while it is open, and the applier refuses every commit and says
  so in every alert for as long as it lasts. An operator with repository admin
  can therefore always get unstuck, which is the point — and can never do it
  quietly, which is the other point.
