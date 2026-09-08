# Work items

Wanted, scoped, deliberately not built yet. Each entry says what it is, why it
is deferred, and what has already been paid for so the next pass does not
re-derive it.

## Three repos, and today only two of them are real

**Decided 2026-09-08 by the user, in these words: "Infra goes in platform.
Truss goes in truss. Recipes go in recipes. VM configuration goes in platform
(this is new and can wait, for now at least if needed)."**

The state that prompted it, measured the same day:

- `beeradb/platform` **already exists and is the real infra repo** — nineteen
  merged PRs, branch protection, a CI plan workflow. Last commit
  2026-09-08 01:03.
- `beeradb/recipes` **still carries a full copy of the same tree** —
  `applier/`, `bootstrap/`, `cluster/`, `credentials/`, `modules/`,
  `platform/`, `projects/`, `vault/`, `tests/`, `providers.allow`. Not an old
  snapshot: a **fork that has since diverged**.
- Both repos carry `engine/`, a stale duplicate of this one. It declares
  `module github.com/beeradb/truss`, holds a single file that is an older
  copy of `internal/gates/gates.go`, and is described in the platform README
  as "in progress and deployed nowhere" — false twice, since truss is
  deployed and it is not from there.

⚠️ **THE DIVERGENCE IS ON THE PRODUCTION SIDE, WHICH IS THE PART THAT
MATTERS.** Everything applied to production after 2026-09-08 01:03 exists only
in the `recipes` copy, and at the time this was written none of it was pushed
anywhere:

| Stranded in `recipes`, absent from `platform` | Status in production |
| --- | --- |
| `applier/manifests/22-truss-cronjob.yaml` | **running** |
| `applier/manifests/23-truss-drift-cronjob.yaml` | **running** |
| `applier/manifests/expiries.json` | **read every daily pass** |
| `vault/grant-publisher.sh` | **already executed against the live Vault** |
| `credentials/vault-snapshot.tf`, `outputs.tf` | applied |
| `vault/41-tailnet-ingress.yaml`, `vault/tailscale/`, `cluster/tailscale/` | written, not applied |

plus divergent copies of `vault/10-vault.yaml`, `20-snapshot-cronjob.yaml`,
`30-networkpolicy.yaml`, `bootstrap-vault.sh`, both `kustomization.yaml`s,
`cluster/00-external-secrets-helm.yaml` and `credentials/variables.tf`.

**So the config for what is running was, briefly, in exactly one place: a
`/tmp` checkout on one VM.** This is the `nohup` crawl-loop failure in another
costume — working fine, and nothing anywhere saying it was one `rm -rf` from
being unreconstructable.

### What has to happen, in this order

1. **Push everything**, before any restructuring. Nothing may be only local.
2. **Port the stranded infra into `platform` as a PR.** ⚠️ Not a direct push:
   that repo has branch protection and a CI plan workflow, and the applier
   gates on plan digests from it. A deadlock caused by an unreportable status
   context has already happened once here.
3. **Delete the infra tree from `recipes`.** Two copies of a live config is
   the defect; deleting the wrong one is how it becomes an outage, so this
   step comes after 2 is merged and verified, never alongside it.
4. **Delete `engine/` from both.** It is this repository, three commits stale.
5. **Move VM configuration into `platform`.** Explicitly "can wait".

⚠️ **Do not treat this as a tidy-up.** The applier reads a repo, CI plans it,
and branch protection gates it — moving roots between repositories re-points
all three, and every one of them fails closed in a way that stops applies.

## Automating the Tailscale credentials

See `docs/decisions/after-launch.md` in the platform repo for the long form.
Short version: `tailscale_oauth_client` is a real provider resource, so the
per-cluster OAuth clients the Kubernetes operator needs can be minted by
`credentials/` with two generations and a real expiry, instead of one console
visit per cluster.

⚠️ The bootstrap client needs the broad `all` scope — the published scope list
has none for managing trust credentials. That is the same concentration
`credentials/` already runs on for `cf-token-mint`.

⚠️ **Minting is not delivery.** A rotated client still has to reach the
cluster's `operator-oauth` Secret. On hetzner-1 that is an ESO
`ExternalSecret`; the Vault cluster has no ESO, so there the Secret is
hand-updated and a rotation will break the operator when the old generation is
destroyed. Settle that before turning rotation on for these.
