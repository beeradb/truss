# Putting the dashboards in an existing Grafana

`import.sh` creates a **Truss** folder in a Grafana you already run and uploads
the metrics dashboards into it, over the HTTP API. It is for the case where you
have a Grafana and a Prometheus already — the ones on the FPL-Armband cluster —
and want truss's dashboards to appear there without standing up anything new.

    GRAFANA_URL=https://your-grafana \
    GRAFANA_PROM=Prometheus \
      observability/grafana/import.sh

- **`GRAFANA_URL`** — the base URL, no trailing slash.
- **`GRAFANA_TOKEN`** — read from the environment, or (preferred) from
  `~/.grafana_token` so it never lands in a shell history or a transcript.
  ⚠️ **It must be an _Admin_ token, not Editor.** Under Grafana 11 RBAC an
  Editor token can create a folder but is granted no permission on it
  (`canSave:false`), so the folder does not list back and every dashboard POST
  into it returns a bare 500. `import.sh` checks for this and stops with a
  clear message rather than leaving three opaque failures. Measured
  2026-09-09.
- **`GRAFANA_PROM`** *(optional)* — the name of the Prometheus datasource to
  bind the dashboards to. Omit it and each dashboard keeps its datasource
  variable, so the viewer picks your Prometheus from the dropdown once.

It is idempotent: the folder is reused, dashboards are overwritten, so
re-running updates them in place.

## What it imports, and what it does not

The three **metrics** dashboards — overview, timeline, and the vault view.

⚠️ **`truss-logs.json` is skipped on purpose.** It queries Loki, which this
path does not deploy; importing it against a Grafana with no Loki would be a
folder of panels that can never render. When a Loki exists, drop the skip.

## This is one of two delivery routes, and they do not overlap

`../../monitoring` (in the platform repo) provisions dashboards through a
Grafana sidecar via `kubectl`, for a cluster you administer. **This** route
needs no `kubectl` — only the URL and a token — which is what makes it the one
that reaches a Grafana on a cluster you do not otherwise touch. Same dashboard
files, two ways in.

## The metrics still have to arrive

The dashboards read truss metrics from Prometheus; truss is a CronJob and
pushes to a Pushgateway that Prometheus scrapes (see `../README.md`). This
script only places the dashboards — it does not carry the metrics. If the
panels read "No data", the gap is the push path, not the import.
