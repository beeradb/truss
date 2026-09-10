# Development

## Layout

    cmd/truss            the pass: gates, queue, apply, rotate, report
    internal/config      every knob, from the environment, validated at start
    internal/forge       the forge: App tokens, branch protection, PRs, reviews
    internal/secrets     the credential mirror, and the expiry sweep
    internal/ledger      object storage: applied/, failed/, HEAD, heartbeat
    internal/plan        exec, the canonical digest, verification
    internal/repo        which roots a commit actually touches
    internal/gates       every refusal, as pure functions over fetched state
    internal/notify      alerting
    internal/metrics     the Prometheus exposition format, and the push
    observability/       dashboards and alerting rules for what it emits

`internal/gates` takes fetched state and returns refusals. It performs no I/O,
and that's the whole point of the package: every decision that can stop a
change is a pure function, so *"an approval on an earlier push does not
count"* is a test that calls a function and reads its answer, rather than one
that drives the whole pass and inspects a ledger object afterward.

`internal/metrics` is split the same way and for the same reason:
`render.go` turns a set of samples into the exposition format and does no
I/O, so "is this push even valid" is a test that calls a function. It matters
more here than it looks -- a Pushgateway answers a malformed body with one
400 for the WHOLE push, so a single bad label would discard every other
metric in the request and leave a status code in a log line nobody reads.

⚠️ **The dashboards and rules in `observability/` name metrics this code
emits, and `cmd/truss/metrics_contract_test.go` fails the build when they
disagree** -- in both directions. Renaming a metric without moving the
dashboard is the `internal/plan/digest.go` hazard again: the panel renders
"No data", which looks exactly like a quiet week.

`internal/secrets` is not a vault client. It reads the applier's credentials
from a mounted mirror, and its one network path lists items and reads a single
metadata field — when each expires. It can read no secret's value and write
nothing.

## Building and testing

    scripts/toolchain install    # Go, jq and OpenTofu, at the pinned versions
    scripts/check

[toolchain.md](toolchain.md) says what those versions are, where each is
pinned, and how to build a machine image or a provisioning pass around them.

That is the whole chain, in the order it must run: build, vet,
`go test -count=1`, govulncheck for reachable standard-library
vulnerabilities, `scripts/toolchain-test` — which proves the installer still
refuses an archive that is not the one this repository pinned —
`scripts/ledger-retention-test` — which drives
`ledger-retention`'s `check-writes` and `lock`: the measurement before an
irreversible retention lock, and the door itself — `scripts/leakscan-test`,
which proves the leak scanner still fails when it should — and then
`scripts/leakscan` itself, which refuses anything identifying a real
deployment.

The three `-test` scripts are there for the same reason: a guard nobody has
watched fail is a claim. The leak scanner has been vacuous in green CI, twice.
`check-writes` shipped exiting zero while printing the objects that forbid
locking, and `lock`'s refusals — a bucket it could not describe, no policy,
and the confirmation that has to name the bucket — are watched here too,
along with the fact that a refusal did not lock anyway, because the door it
opens does not close. `scripts/toolchain-test` is the same argument applied to
what fetches the toolchain: an archive that is not the one this repository
pinned has to be refused before anything is installed, and that refusal is
watched rather than assumed. See [toolchain.md](toolchain.md).

CI runs the same script. A chain stated in two places drifts, and the reason
for each step is written beside the command rather than here.


This repository is public and the platform it manages is not — that's the whole
risk. A live deployment has real account ids, bucket names, hostnames and vault
names in it, and any of them can reach a fixture or a comment by being pasted
somewhere convenient. `scripts/leakscan` scans for *classes* of thing rather
than a list of real values, because a denylist of somebody's actual secrets
would itself be the leak. CI runs it on every push.

It has its own test, because a guard nobody has watched fail is a claim, not
a check.
