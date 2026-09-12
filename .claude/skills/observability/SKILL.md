---
name: observability
description: Add or change an alerting rule, a dashboard panel, or a metric truss emits. Use whenever the answer to "is it working" needs a query — and before believing a panel that shows nothing.
---

# Observability

## The failure mode is silence that looks like health

A panel whose expression does not parse renders **"No data"**, which is exactly
what a quiet week looks like. An alerting rule Prometheus cannot load is worse:
the whole file is rejected, so one bad expression disarms every other rule
beside it. Nothing about either is loud.

## Two checks, and they do not overlap

- `cmd/truss/metrics_contract_test.go` checks that the metric **names** a panel
  mentions are names truss actually emits. An unparseable query with correct
  names passes it.
- `scripts/check-observability` parses every alerting expression **and every
  dashboard panel expression** with Prometheus's own parser. promtool has no
  "check this dashboard" mode, so each panel `expr` is wrapped in a throwaway
  recording rule and handed to the parser Prometheus uses at load time. The
  wrapper is what makes it a check.

Run both. `scripts/check` runs them in order.

⚠️ **Without promtool the script SKIPS, and a skip reads as a pass.** Set
`TRUSS_REQUIRE_PROMTOOL=1` locally as CI does, or you have not checked the half
of this directory that nothing else validates.

## Report the counter that moves

A job that speaks only when it fails cannot be told apart from a job that has
died. The heartbeat is written on success too, and a new metric needs its
success path emitted, not just its error path. When you add a series, add the
panel or rule that would notice it going absent — a gauge nobody queries is a
gauge nobody misses.

## A panel is only as real as the scrape behind it

A dashboard in this repository is a query; whether anything answers it depends
on the consumer's scrape config. A scrape job applied by hand exists in no
repository, so re-applying the monitoring stack silently drops it and every
panel goes quiet with no error anywhere — that is not hypothetical, it has
happened to truss's own job in a live deployment. If a panel needs a new job,
label or target, ship it in the manifests this repository hands over.

## Watch it fail

Break the expression you just wrote — a stray paren is enough — run
`scripts/check-observability`, and read the message. It must name the rule or
the panel you broke. Restore, run it green, then `/watch-it-fail` for the
general procedure. `observability/grafana/import.sh` is a convenience for
looking at a dashboard; it is not a deploy path and proves nothing.
