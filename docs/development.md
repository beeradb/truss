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

`internal/gates` takes fetched state and returns refusals. It performs no I/O,
and that's the whole point of the package: every decision that can stop a
change is a pure function, so *"an approval on an earlier push does not
count"* is a test that calls a function and reads its answer, rather than one
that drives the whole pass and inspects a ledger object afterward.

`internal/secrets` is not a vault client. It reads the applier's credentials
from a mounted mirror, and its one network path lists items and reads a single
metadata field — when each expires. It can read no secret's value and write
nothing.

## Building and testing

    go build ./... && go vet ./... && go test ./...
    scripts/leakscan          # refuses anything identifying a real deployment
    scripts/leakscan-test     # proves leakscan still fails when it should

This repository is public and the platform it manages is not — that's the whole
risk. A live deployment has real account ids, bucket names, hostnames and vault
names in it, and any of them can reach a fixture or a comment by being pasted
somewhere convenient. `scripts/leakscan` scans for *classes* of thing rather
than a list of real values, because a denylist of somebody's actual secrets
would itself be the leak. CI runs it on every push.

It has its own test, because a guard nobody has watched fail is a claim, not
a check.
