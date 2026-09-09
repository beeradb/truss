# Opening the armband Grafana to the tailnet

`console.fplarmband.com` is Traefik on the public IP with an **IP allowlist**
(home IP only), so an off-network node gets a bare 403 that no credential can
satisfy. Putting Grafana on the tailnet sidesteps that: the tailnet ACL
governs access instead of the allowlist, and it never touches the public edge.

Two ways, both needing access to the cluster/box that this agent does not
have — so one of them is yours to run, and then the dashboard import (which is
just `curl`, and is not gated) is mine.

## Route A — the operator (k3s-native, matches the vault cluster)

`grafana-tailnet-ingress.yaml` here, once the Tailscale operator is installed
(its header has the rendered-not-HelmChart install, and why). Confirm the
three ⚠️ fields point at your real Grafana Service.

    kubectl --context <armband> apply -f grafana-tailnet-ingress.yaml

Then a tagOwners + ACL entry in the tailnet policy:

    "tagOwners": { "tag:grafana": ["tag:k8s-operator"] }
    { "action": "accept", "src": ["100.89.238.109"], "dst": ["tag:grafana:443"] }

The served URL becomes `https://grafana.tail4eb13f.ts.net`.

## Route B — `tailscale serve` on the box (one command, no operator)

If Grafana is reachable at `localhost:3000` on the box and tailscaled runs
there:

    tailscale serve --bg 3000        # -> https://hetzner-1.tail4eb13f.ts.net

plus the same ACL entry with `dst: ["hetzner-1:443"]`.

## Then the part that is mine

A Grafana **Admin** token at `~/.grafana_token`, and I run:

    GRAFANA_URL=https://grafana.tail4eb13f.ts.net \
      observability/grafana/import.sh          # Route A
    # or GRAFANA_URL=https://hetzner-1.tail4eb13f.ts.net for Route B

which creates the **Truss** folder and lands the three metrics dashboards,
bound to that Grafana's Prometheus. `import.sh` is proven and idempotent.

⚠️ My tailnet node is `100.89.238.109` (`agent`), which is what the ACL must
admit. Without that ACL entry the endpoint is reachable by the operator but
still refused to me.
