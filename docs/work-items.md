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

## A local `truss` you can point at a stuck applier

**Asked for 2026-09-08, in these words: "a binary on my computer I can use to
inspect the queue and unstick things."**

The applier was stuck for most of a day, and every step of understanding and
repairing it was hand-written:

- reading the ledger watermark meant a boto3 script and a 1Password lookup
- learning WHY a commit was refused meant kubectl logs and pattern-matching an
  error message
- advancing the watermark past a commit that could never apply meant a second
  hand-written script, pasted into a terminal, twice
- and both scripts failed the first time on an S3 checksum incompatibility
  that `applier/apply.sh` already documents and neither script knew about

None of that is exotic. It is `truss ledger get`, a `status`, and a guarded
`skip` -- over machinery truss already has: the ledger client, the S3 signing,
the plan digest, the config loading. What is missing is a front door.

Sketch, in the order today wanted them:

    truss status          what commit the applier is on, what HEAD is, the gap
                          between them, and if it is refusing something, WHY --
                          the digest comparison in words rather than two hashes
    truss ledger get      the watermark, without a boto3 script
    truss why <sha>       the recorded failure for one commit
    truss skip <sha>      advance past a commit that cannot apply, refusing
                          unless the reason is one it recognises, and writing
                          the break-glass record itself

⚠️ **THE SKIP IS THE DANGEROUS ONE AND MUST BE THE HARDEST.** Advancing the
watermark is editing the applier's memory of what it has done, by hand, out of
band. It was the right call twice on 2026-09-08 and it is exactly the
operation that, made convenient, gets reached for INSTEAD of understanding a
failure. It should demand the reason, record it, and refuse a commit whose
plan the applier has never actually tried.

⚠️ **AND IT MUST NOT NEED A CLUSTER.** The whole value is working from a
laptop when the cluster is the broken thing -- so it reads the ledger over S3
with the same credential the applier uses, never through kubectl.

⚠️ **It also inherits the checksum workaround, which is a reason to build it
rather than keep writing scripts.** Both hand-written scripts died on
`SignatureDoesNotMatch ... Invalid argument` against GCS -- which reads like a
bad credential and is not: recent botocore sends checksum headers Google's S3
API rejects. truss's own ledger client already handles it. In a CLI that
knowledge lives in one place instead of being rediscovered at 2am.
