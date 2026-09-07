# Truss

**A GitOps applier where the diff a human approved is provably the diff that
ran**, and where every credential it uses is either hand-made because nothing
could mint it, or minted and rotated by code on a clock.

A truss distributes load and stops a frame racking. This one carries trust:
it re-checks, from scratch and on every pass, that the change in front of it
was reviewed under rules it verifies rather than assumes — then plans the
approved commit with its own credentials and refuses to apply anything whose
plan does not match the one the reviewer read.

## What it does that other GitOps setups do not

Most let a human approve a *preview*, then let a robot compute a *fresh* plan
at apply time and run that instead. Those are two different plans and nothing
checks that they agree; between them the world moves.

Truss ties them together with a hash. CI plans once, renders the comment you
read from that same plan file, and publishes a canonical digest of it. The
applier plans the approved commit again with its own credentials and refuses
any root whose plan does not hash to what you approved.

⚠️ The direction is the point. CI is deliberately read-only, so its artifact
is a **witness, not an instruction** — a compromised CI can force a *refusal*
and can never cause an apply. Handing the applier a plan file to execute
would invert exactly that.

Also: every credential is a root or it is minted, with no third kind. A leak
is answered by a commit. Drift is caught daily, without an adversary. The
applier re-reads the branch protection it depends on and refuses to run if it
has been weakened.

## Status

⚠️ **Being extracted, not yet extractable.** The running implementation is
915 lines of bash in the platform repo this directory still lives in; Truss is
its replacement, ported piece by piece against that reference, and nothing
here is deployed. `internal/gates` is the first real package — every refusal
in the system as pure functions over fetched state, so "an approval on an
earlier push does not count" is a function call rather than a whole script
driven against stub binaries.

See `docs/decisions/engine-extraction.md` for the split, what has to become
configuration before this can be published, and why the reason is
distribution rather than blast radius.
