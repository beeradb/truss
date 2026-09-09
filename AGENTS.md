# Working in this repository

Truss decides whether somebody else's infrastructure changes. A defect here
is not a bug in a tool — it is an apply that should have been refused, or a
refusal nobody can diagnose at 3am. The rules below are not style preferences;
each one is here because its absence cost something.

Read [docs/development.md](docs/development.md) for the layout and the build.
This file is about *how to work*, and the two questions it keeps answering are
**"how do you know?"** and **"where does this live?"**

---

## Do it once, the right way

**One representation where one will do.** When something appears twice, the
fix is not to keep the copies in step — it is to have one. Two copies of a
rule disagree eventually, and the one you are reading is the stale one.

⚠️ **The digest filter is the exception that proves it, and it is a warning
rather than a licence.** `internal/plan/digest.go` reproduces a jq program
that also exists in the consumer's `applier/plan-digest`, deliberately, so a
diff between them is readable. That duplication has already cost a
production outage: the filter hashed `no-op` entries whose values differ
between two identities, both sides had to change together, and until both
did, *every* apply was refused. If you touch one, you are touching two.

**Refuse rather than guess.** Every gate in `internal/gates` returns a refusal
with a reason. There is no path that proceeds on a default when the answer is
unknown, and adding one is the single most damaging change you can make here:
a gate that fails open is worse than no gate, because it reports success.

**No fallbacks.** One correct path. A fallback is a second path nobody tests,
taken only when the first has already failed — which is the worst moment to
run code nobody has exercised.

**Unset configuration is an error, not a default.** `internal/config` validates
at start and names every problem. A missing credential must stop the pass
before it touches anything, not surface eight steps later as a confusing
permission error.

---

## Comments explain *why*, and carry their evidence

The comments in this repository are long on purpose. A comment that says what
the code does is noise; a comment that says **why this value, why not the
obvious alternative, and what happened when we tried it** is the only durable
record of a decision.

Where a value was calibrated, the measurement stays in the comment. Where a
bug was fixed, the failure stays. Do not strip them for brevity — several are
the only surviving evidence for a choice that looks arbitrary and is not.

⚠️ **A correction REPLACES the claim. It does not narrate the replacement.**
Delete what turned out to be false rather than annotating it as withdrawn.
These files are read for the current state, and the history of how they got
there crowds it out.

---

## Guards are tests, not confirmations

**A check nobody has watched fail is a claim.** Before trusting a new guard,
break the thing it guards and watch it go red. `scripts/leakscan-test` exists
for exactly this reason, and it has caught an over-broad exemption that would
have let a private repository's URL through.

⚠️ **Write the guard as a test, never as a comment asserting the guard
holds.** A rule written down and not tested is probably already false. Three
found this way: a plan digest that hashed values varying by *who ran the
plan*; a `NetworkPolicy` admitting a whole namespace, forbidden by a test that
nothing ran; a suite asserting invariants about the applier that had been
superseded months earlier.

**Gate on the field, never on rendered text.** A test matching
`name: LEDGER_BUCKET, value: "..."` with a regex broke when the identical
setting was written in block-style YAML, and reported "the CronJob does not
set LEDGER_BUCKET" about a CronJob that set it on the next line. Parse the
structure and read the value.

**A fixture more forgiving than production invents failures; one stricter
hides them.** When a fixture and production disagree, the fixture is the bug.
A suite that is "just flaky" is a claim about an instrument nobody has
checked.

**Tests must not depend on the machine that runs them.** A test passed here
for months only because the developer's laptop happened to have a kubectl
context named `vault`. If a test needs a context, a binary or a fixture, it
must supply it.

---

## Architecture

**CI is a witness, never an instruction.** CI plans and publishes a digest;
the applier re-plans and refuses a mismatch. A compromised CI can cause a
*refusal* and never an apply. Any change that lets CI decide what runs — an
artifact it builds and the applier executes, a status it sets that grants
rather than blocks — has to preserve that direction, and the way to preserve
it is to pin the artifact by digest inside the reviewed diff.

**Pin artifacts by digest, never by tag.** A tag is mutable, so what ran last
night becomes unanswerable. Both the truss image and the consumer's provider
mirror are pinned by `@sha256:`, and the consumer's own tests refuse a
tag-only reference.

**Ship what the consumer cannot make, and nothing more.** Truss publishes a
binary and an image containing `tofu`, `git` and `op` — the tools it executes.
It carries no provider mirror, because a mirror is built from the *consumer's*
terraform config. Baking one in coupled a general tool to one deployment and
made "add a provider" mean "rebuild the applier by hand on one box".

⚠️ **A component that cannot deliver its own fixes is the failure that
outlasts every other one.** The applier could not deploy itself, so every
repair needed a human on a specific machine — and the fix for that could not
be shipped *by* the applier. Prefer designs where the thing being changed
arrives the same way every other change does.

**Anything that must be undone is registered before it is done.** Traps are
installed before the state they clean up is created, not after; a failure
between the two is exactly the case the trap exists for. Bash keeps only the
last `EXIT` trap, so there is one handler, not several.

---

## Working on the pass itself

`internal/gates` performs no I/O. Every decision that can stop a change is a
pure function over fetched state, so *"an approval on an earlier push does not
count"* is a test that calls a function, not one that drives a whole pass and
inspects a ledger afterward. Keep new refusals there.

**Report the counter that MOVES.** A batched job that reports only on
completion cannot be told apart from a hung one. The pass narrates as it goes
and writes a heartbeat on success as well as failure — a job that writes a
file only when it fails cannot be distinguished from one that is no longer
running.

**Errors name what failed, not a plausible neighbour.** A generic
`except`/`if err != nil` that reports a specific step is claiming to know
something it did not check. One such message told a user their recipe had
failed to archive when it had been archived, and a lock elsewhere was the real
cause.

---

## Before you commit

    go build ./... && go vet ./... && go test -count=1 ./...
    scripts/leakscan

⚠️ **`-count=1` is not optional.** Cached results have twice been read as a
passing run over code that did not compile.

⚠️ **Never chain a test run and a commit.** `go test … && git commit` runs the
commit whichever way the tests went, and `go test` reads the working tree
while `git commit` reads the index. Verify what will actually ship.

⚠️ **This repository is public and the infrastructure it manages is not.**
`scripts/leakscan` refuses account ids, bucket names, hostnames, vault URIs
and anything else identifying a real deployment. It scans for *classes*, not a
denylist of real values, because such a list would itself be the leak. If it
refuses something you believe is safe, narrow the exemption to the exact
public thing — never to its host in general — and add both a passing and a
refusing case to `scripts/leakscan-test`.

**No AI attribution in commits.** No `Co-Authored-By` trailers naming an
assistant, no "generated with" footers. A commit-msg hook enforces it and
`leakscan` reads history for it.
