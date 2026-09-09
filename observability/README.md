# Observability

Truss reports what it did, every pass, as Prometheus metrics — and the three
Grafana dashboards and the alerting rules in this directory read them.

    alerts/truss.rules.yml            every alert, with the reasoning inline
    dashboards/truss-overview.json    is it alive, what did it do, what broke
    dashboards/truss-timeline.json    what state it was in, when
    dashboards/vault.json             the credential store, and what truss can read of it

Nothing here is deployment-specific: no host, no bucket, no vault name. The one
thing you supply is where to push.

## ⚠️ Read this before writing a single query

**Truss is a CronJob. It pushes; nothing scrapes it.** The pass exists for the
length of one run, so by the time a scrape arrived the process holding the
numbers has exited. It pushes to a **Prometheus Pushgateway**, which is the
component for exactly this case.

Three consequences, and none of them are optional reading:

**1. A Pushgateway serves the last thing it was given, forever.** If truss
stops running entirely — the image will not pull, the CronJob was suspended,
the node is gone — every metric here keeps answering with whatever the last
healthy pass said. `truss_pass_success` stays `1`. A dashboard built on the
outcome series alone reports a dead applier as a healthy one.

> **`time() - truss_pass_timestamp_seconds` is the only expression that goes
> bad on its own when nothing pushes.** It is the dead man's switch, it is the
> first rule in `alerts/truss.rules.yml`, and it is the first tile on the
> overview dashboard. Anchor on it; read nothing else as evidence when it is
> red.

**2. Every sample is a gauge, and `rate()` does not mean what it usually
means.** A counter's meaning is "monotonically increasing since this process
started"; this process starts, counts to three and exits, so the next pass
would push a smaller number and Prometheus would read the drop as a counter
reset. What truss can honestly report is the state of **one pass**.

So `truss_pass_commits_applied` is "the last pass applied N commits", not a
running total. `increase(...[1d])` over it is wrong twice over: the gateway is
scraped every 15 seconds and serves the same value each time, so the same pass
is counted repeatedly. Query these as state — `truss_digest_refusals > 0`, not
`rate(truss_digest_refusals[5m])`.

**3. The two passes push under different grouping keys.** The frequent pass
(every five minutes) applies commits and never rotates; the daily pass rotates,
publishes and sweeps expiries and never applies. They arrive as
`pass="frequent"` and `pass="drift"`. Under one key the frequent pass would
overwrite the daily one's rotation and expiry series five minutes after they
were written, so **a query for anything rotation- or expiry-shaped must say
`{pass="drift"}`**.

## Wiring it up

### 1. Point truss at a gateway

`METRICS_PUSH_URL` is the gateway's root. Optional with no default: unset, the
feature is off and nothing about the pass changes.

```yaml
- name: METRICS_PUSH_URL
  value: http://pushgateway.monitoring.svc:9091
```

Set it on **both** CronJobs — the frequent one and the daily drift one. A
gateway that is down or wrong cannot fail a pass: the push is the last thing a
pass does, its failure is a non-fatal `level=warn` line, and the alert and the
heartbeat have already been written by then.

⚠️ **It is a bearer secret in the same sense the dead-man's-switch ping URL
is.** A gateway behind basic auth carries its credentials in the URL's
userinfo. Truss never logs it, never echoes it into an error, and never renders
it into the heartbeat or the chat message. Keep it out of anything else that
prints its environment.

### 2. Run a gateway

```yaml
# The gateway holds state in memory. --persistence.file survives a restart;
# without it, a restarted gateway serves nothing until the next pass pushes,
# and TrussIsNotReporting fires — which is correct, and avoidable.
args: ["--persistence.file=/data/pushgateway.store", "--persistence.interval=1m"]
```

### 3. Scrape it — with `honor_labels: true`

```yaml
scrape_configs:
  - job_name: pushgateway
    honor_labels: true          # ⚠️ NOT OPTIONAL. See below.
    static_configs:
      - targets: ['pushgateway.monitoring.svc:9091']
```

⚠️ **Without `honor_labels: true` nothing in this directory works, and nothing
says so.** The gateway derives `job="truss"` and `pass="frequent"` from the
push URL's path and exposes them as labels on every series. Prometheus's
default is to overwrite a target's `job` label with the scrape job's name — so
every series arrives as `job="pushgateway"`, every selector in the rules and
dashboards matches nothing, and every panel renders "No data" exactly as though
truss were quiet. Set it once; check one panel.

### 4. Load the rules and the dashboards

The rules file is a standard Prometheus `groups:` document — mount it wherever
your Prometheus reads rules from, or paste it into a `PrometheusRule` CR's
`spec` if you run the operator.

The dashboards import as-is. Each declares a `datasource` template variable
rather than baking a datasource UID, so they land in any Grafana without
editing; pick your Prometheus from the dropdown on first open.

## Alerts

`alerts/truss.rules.yml` carries its reasoning inline. The shape of it:

| Group | Fires when |
|---|---|
| `truss-liveness` | no pass in 15 minutes; no daily pass in 30 hours; no metrics at all |
| `truss-refusals` | a plan did not hash to the approved one; a commit did not pass the commit gate; branch protection or a ruleset no longer meets the bar |
| `truss-failures` | an apply, plan or credential failure; a ledger object that could not be written; an error-level log line; a pass slower than its own cadence |
| `truss-credentials` | a credential expired, expiring within 14 days, or recording no expiry; a sweep that could not run |
| `truss-rotation` | rotation failed; rotation is not running at all; the publisher did not confirm the write |
| `truss-drift` | a root drifted; a root whose drift could not be checked |

Two of these deserve naming outright:

**`TrussPlanDigestRefused` is the alert this project exists to send.** The plan
truss re-planned did not hash to the plan a human approved. It repeats every
pass until somebody resolves it — the queue does not advance past a refused
commit — so it stays firing rather than flapping once and resolving itself.

**`TrussRotationIsNotRunning` catches the silence.** A rotation that *fails*
alerts loudly. A rotation that is *skipped* — no credentials root, the state
lock held elsewhere — reports no failure at all, and the applier looks entirely
healthy while the 45-day clock keeps running.

## What truss emits

Every series carries `job="truss"` and `pass="frequent"|"drift"` from the
grouping key, on top of the labels below.

| Metric | Labels | What it is |
|---|---|---|
| `truss_pass_timestamp_seconds` | | when the pass finished — **the liveness signal** |
| `truss_pass_duration_seconds` | | how long it took, gate to alert |
| `truss_pass_success` | | 1 when the pass reported no failure |
| `truss_pass_failure` | `class` | 1 per class of thing that went wrong; a pass can set several |
| `truss_pass_commits_applied` | | commits this pass applied |
| `truss_pass_commits_noop` | | commits recorded as touching no root |
| `truss_pass_lock_contended` | | 1 when another holder had the state lock |
| `truss_pass_ledger_errors` | | ledger objects that could not be written |
| `truss_pass_log_events` | `level` | lines logged at `warn` and `error` |
| `truss_gate_ok` | `gate` | 1 when `protection` / `rulesets` met the bar |
| `truss_digest_checks` | | roots whose digest was compared against the approved one |
| `truss_digest_refusals` | | roots refused because the plan did not match |
| `truss_root_duration_seconds` | `root`, `phase` | seconds in `init`, `plan`, `show`, `apply`, `drift` |
| `truss_root_resource_changes` | `root` | resource changes in the plan that was applied |
| `truss_root_failures` | `root` | times this root failed or was refused |
| `truss_root_drifted` | `root` | 1 per root that no longer matches its configuration |
| `truss_root_drift_errored` | `root` | 1 per root whose drift could **not** be checked |
| `truss_drift_ran` | | 1 when the pass actually planned every root |
| `truss_rotation_ran` | | 1 when the credentials root was re-applied |
| `truss_rotation_ok` | | 1 when rotation reported no error |
| `truss_rotation_changes` | | resource changes rotation made — above zero only on a boundary day |
| `truss_publish_attempted` | | 1 when the publisher sidecar was contacted |
| `truss_publish_ok` | | 1 when it answered without an error |
| `truss_publish_expiries` | | expiry dates the publisher recorded |
| `truss_expiry_sweep_ok` | | 1 when the daily sweep completed and **earned** its answer |
| `truss_expiry_findings` | | credentials the sweep reported |
| `truss_credential_days_left` | `credential` | days until expiry; negative means it went |
| `truss_credential_expiry_unrecorded` | `credential` | 1 per credential recording no expiry at all |
| `truss_build_info` | `go_version`, `revision` | always 1; the labels are the payload |

### The three that are easy to misread

**`truss_expiry_sweep_ok` has to be read before any finding.** The sweep
refuses to claim a clean bill it did not earn — but a sweep that *could not
run* reports no findings, which is exactly what a clean sweep looks like. Zero
findings means nothing until this is `1`.

**`truss_credential_expiry_unrecorded` is not "does not expire".** `never` is a
recorded value and does not appear here. What appears here is a credential
whose lifetime nothing is watching, so its lapse gets discovered by an outage.

**`truss_root_drifted` and `truss_root_drift_errored` are different claims.**
"We looked and it drifted" and "we could not tell" are not the same fact, and
the second is the one that hides a broken provider credential. Nothing folds
them together; neither should a panel.

### The failure classes

`truss_pass_failure{class="..."}` exists because the alert text is a sentence
and a rule cannot filter on a sentence — one that tried would be a regex over
prose. Every class is pushed every pass, most of them as `0`, so a query
returning "no data" means *nothing pushed* rather than *nothing went wrong*.

| Class | Means |
|---|---|
| `protection` | branch protection does not meet the bar |
| `rulesets` | a ruleset does not meet the bar |
| `commit` | a commit did not get to `main` the way it must have |
| `digest` | **the plan did not hash to the approved one** |
| `plan` | `tofu init` / `plan` / `show` failed |
| `apply` | `tofu apply` failed |
| `credentials` | a credential the apply needs is missing or unreadable |
| `repo` | the working copy could not be cloned, fetched or read |
| `forge` | the forge would not answer, or would not mint a token |
| `rotation` | rotating the credentials root failed |
| `publish` | the publisher handoff failed |
| `ledger` | a ledger object could not be written |
| `config` | the repository does not contain what it says it does |

`digest` and `commit` are the two that mean something got past the review path.
The rest are operational. They are never collapsed into one severity, because
they want different people.

## Logs

Every pass narrates to **stderr in logfmt**, with a level:

    time=09:31:04 level=info msg="considering 4f2a91c"
    time=09:31:07 level=info msg="plan for platform matches the one approved at 4f2a91c"
    time=09:32:15 level=warn msg="telegram send failed (non-fatal): ..."

`level=warn` is something that was noticed and survived. `level=error` is
something that was **lost** — a ledger object that was not written, a durable
record that no longer exists.

Both are counted into `truss_pass_log_events{level=...}`, so the error and
warning panels work with no log pipeline at all. If you do run one (Loki,
Alloy, anything that parses logfmt), the same field is what a
`| logfmt | level="error"` query selects on, and the panels get their detail
back.

⚠️ **The message is one quoted prose value on purpose.** Structured attributes
per call site — `commit=`, `root=`, `duration=` — remain the entry in
`docs/work-items.md` they were; this change adds the level field the panels
filter on and does not pretend to be that work.

## The vault dashboard

`dashboards/vault.json` is in two halves.

The **top** is Vault's own telemetry, written from Vault's documented metric
names. Vault emits none of it until its config has

```hcl
telemetry {
  prometheus_retention_time = "24h"
  disable_hostname          = true
}
```

and the endpoint is readable by the scraper — either
`unauthenticated_metrics_access = true` on the listener's `telemetry` block, or
a scrape carrying a token with `read` on `sys/metrics`. Verify the names once,
from somewhere that can reach it:

    curl -s "$VAULT_ADDR/v1/sys/metrics?format=prometheus" | grep '^# TYPE'

Running **OpenBao**? Every metric is `bao_*` rather than `vault_*` — one
find-and-replace on the JSON.

The **bottom row** is truss's own view of that vault, and is the part no
generic Vault dashboard can give you: whether the applier's daily read
completed, and how long the credentials it can see have left. The applier's
role can list an item and read one metadata field — when it expires — and
nothing else. It cannot read a secret's value and it cannot write one, so that
row is the whole of what it can honestly say.

## Keeping this directory honest

A dashboard that names a metric the code no longer emits renders "No data",
which looks exactly like a quiet week. `cmd/truss/metrics_contract_test.go`
fails the build when that happens, in both directions:

- every `truss_*` series a dashboard or rule names must be one `passMetrics`
  can emit;
- every series `passMetrics` emits must be named by at least one dashboard or
  rule — an unwatched metric is one nobody will notice the absence of either.

So renaming a metric fails `go test` until the artifacts here move with it.
This is the same hazard `internal/plan/digest.go` carries against the
consumer's `plan-digest` jq, answered the same way.
