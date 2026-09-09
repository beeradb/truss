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

    go build ./...
    go vet ./...
    go test -count=1 ./...
    go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...  # reachable stdlib vulnerabilities
    scripts/leakscan          # refuses anything identifying a real deployment
    scripts/leakscan-test     # proves leakscan still fails when it should
    scripts/check-observability   # parses every alerting rule and panel query

`check-observability` needs `promtool` from a Prometheus release and skips
loudly without it; CI sets `TRUSS_REQUIRE_PROMTOOL=1`, which turns that skip
into a failure — the same shape the jq differential test already uses. It
exists because `go test` cannot tell a valid query from an invalid one that
happens to name real metrics: a broken panel renders "No data", which is what
a quiet week looks like, and a broken rule makes Prometheus reject the whole
file and silently disarm every rule beside it.

This repository is public and the platform it manages is not — that's the whole
risk. A live deployment has real account ids, bucket names, hostnames and vault
names in it, and any of them can reach a fixture or a comment by being pasted
somewhere convenient. `scripts/leakscan` scans for *classes* of thing rather
than a list of real values, because a denylist of somebody's actual secrets
would itself be the leak. CI runs it on every push.

It has its own test, because a guard nobody has watched fail is a claim, not
a check.
