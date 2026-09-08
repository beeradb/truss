# Development

## Layout

    cmd/applier          the pass: gates, queue, apply, report
    internal/config      every knob, from the environment, validated at start
    internal/forge       the forge: App tokens, branch protection, PRs, reviews
    internal/secrets     the vault: batched reads, rate-limit backoff
    internal/ledger      object storage: applied/, failed/, HEAD, heartbeat
    internal/plan        exec, the canonical digest, verification
    internal/gates       every refusal, as pure functions over fetched state
    internal/notify      alerting

`internal/gates` takes fetched state and returns refusals. It performs no
I/O, and that's the whole point of the package: in the bash version these
decisions were tangled up with the API calls that fed them, so testing *"an
approval on an earlier push does not count"* meant driving the entire script
against stub binaries and reading a ledger object afterward. Here it's a
function call.

Of that layout, `internal/gates` exists. The rest is the plan.

## Building and testing

    go build ./... && go vet ./... && go test ./...
    scripts/leakscan          # refuses anything identifying a real deployment
    scripts/leakscan-test     # proves leakscan still fails when it should

This repository is written to be public, and it's ported from a private one —
that's the whole risk. The reference implementation is a live platform with
real account ids, bucket names, hostnames, and vault names in it, and porting
is fundamentally a copying exercise. `scripts/leakscan` scans for *classes* of
thing rather than a list of real values, because a denylist of somebody's
actual secrets would itself be the leak. CI runs it on every push.

It has its own test, because a guard nobody has watched fail is a claim, not
a check.
