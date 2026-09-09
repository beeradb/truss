# Working in this repository

Truss decides whether somebody else's infrastructure changes. A defect is an
apply that should have been refused. [CLAUDE.md](CLAUDE.md) has the
principles; these are the repo-specific ones.
[docs/development.md](docs/development.md) has layout and build.

- **Every gate returns a refusal with a reason.** A gate that fails open is
  worse than none: it reports success. New refusals go in `internal/gates`,
  which does no I/O so each is testable by calling it.
- ⚠️ **`internal/plan/digest.go` duplicates the consumer's `plan-digest` jq
  deliberately.** Both must change together; when they did not, *every* apply
  was refused until both did.
- **A check nobody has watched fail is a claim.** Break it, watch it go red,
  then trust it.
- **Gate on the field, never rendered text.** A regex expecting flow-style
  YAML once reported "does not set LEDGER_BUCKET" about a manifest that set it
  on the next line.
- **When a fixture and production disagree, the fixture is the bug.** And a
  test must not depend on the machine running it — one passed for months only
  because a laptop had a kubectl context named `vault`.
- **CI is a witness, never an instruction.** CI publishes a digest; the applier
  re-plans and refuses a mismatch. Artifacts CI builds are pinned by digest
  inside the reviewed diff — a tag makes "what ran last night" unanswerable.
- **Ship what the consumer cannot make.** Truss carries no provider mirror:
  that is built from their config, and baking it in coupled a general tool to
  one deployment.
- ⚠️ **Prefer designs where a component can deliver its own fixes.** The
  applier could not, so every repair needed a human on one machine —
  including the repair for that.
- **Comments carry their evidence**: the measurement that set a value, the
  failure that caused a fix. A correction replaces the claim rather than
  narrating it.
- **Report the counter that moves.** A job reporting only on completion cannot
  be told apart from a hung one; the heartbeat is written on success too.

Before committing:

    go build ./... && go vet ./... && go test -count=1 ./... && scripts/leakscan

⚠️ `-count=1` is not optional — cached results have twice passed over code
that did not compile. Never chain a test run and a commit: it commits either
way, and reads a different tree than it tested.

⚠️ **This repository is public; the infrastructure it manages is not.**
`leakscan` refuses classes of identifier, not a denylist of real values —
that list would itself be the leak. Narrow an exemption to the exact public
thing, never its host, and add a passing *and* a refusing case to
`leakscan-test`. No AI attribution in commits.
