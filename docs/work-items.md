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

---

# From the 2026-09-09 survey of other appliers

Seven parallel research passes over competing appliers (Atlantis, Spacelift,
HCP Terraform, env0, Scalr, Terrateam, Digger, Terragrunt), the Kubernetes
GitOps controllers, the provenance and transparency-log standards, and the
published post-incident guidance. What follows is only what survived checking
against this tree. Ideas that did not survive are recorded at the bottom, so
the next pass does not re-derive them.

⚠️ **The headline of the survey is that nothing else does the central thing.**
Every competitor applies the plan artefact it just produced, in one process,
with one credential set: Atlantis applies its own `$PLANFILE`, HCP and
Spacelift gate between plan and apply inside a single run object, and
Digger/Terrateam run both on the same CI runner by design. The closest prior
art is not in this ecosystem at all — it is reproducible builds, where an
independent rebuilder re-derives an artefact and compares. This applier is a
rebuilder of one, and its re-derivation runs with the *privileged* identity
rather than a sandboxed approximation, which is why it caught the
read-only-vs-admin no-op divergence a byte-comparison rebuilder has no
category for.

## A partially applied multi-root commit wedges the queue

**FIXED 2026-09-09.** `cmd/truss/apply_partial_multiroot_test.go` reproduced
it first and is kept as the guard. What follows is the mechanism, because the
fix is a narrowing of the digest gate and the argument has to survive it.

`applyOneRoot` runs init, plan, digest-compare and **apply** for one root
before the next root is planned (apply_cmd.go:645), and `applied/<sha>` plus
`AdvanceHead` are written only after every root in the commit has succeeded.
So a failure in root 2 leaves root 1 applied to real infrastructure with HEAD
still on the previous commit.

The next pass re-derives the same commit and re-plans root 1 against
infrastructure that now already carries root 1's changes. That plan is a
no-op, `Canonical` drops no-ops, and the digest is the digest of `[]`. CI
filed the digest of a plan that changed something. They cannot match, and the
refusal that comes out says *"the world moved between review and apply"* —
which is not what happened. Every later pass repeats it identically.

    pass two: the plan for platform does not match the one approved at
    multirootsha (approved 5ad51e49…, ours 37517e5f…): the world moved
    between review and apply

⚠️ **The shared-input case is the common one, not the exotic one.** Touching
`modules/`, `providers.allow` or `.opentofu-version` plans EVERY root
(`repo.TouchedRoots`), so a provider bump is always a multi-root commit.

**The fix: a plan that changes nothing is not digest-gated, because there is
nothing to gate.** The digest proves that what is about to CHANGE is what the
approver read; a plan with no changes in it changes nothing, so it cannot
deviate from what was approved and an attacker gains nothing — the apply is a
no-op either way. It is the same argument `plan.Canonical` already makes for
dropping individual no-op resources, applied to a plan that is entirely
no-ops. It also settles the mirror-image case, which was equally stuck: a
change somebody had already made by hand, exactly as approved, left an empty
plan that was refused forever rather than recorded as already satisfied.

⚠️ **An UNREADABLE plan is still gated.** `countResourceChanges` returns
`(0, false)` for a plan it cannot parse, and treating that as "no changes"
would skip the gate on exactly the input nobody understands — absent reading
as compliant, which `internal/gates` exists to keep out. Both halves are
asserted in `TestAPlanThatChangesNothingIsNotGated`.

⚠️ **Two fixtures were wrong and hid this.** `fakeTofu`'s default `show -json`
was `{"resource_changes":[]}`, so every digest-gate unit test drove a plan
that applies nothing — the one input the gate cannot apply to. The default now
changes something, and the tests were re-checked by deleting the gate and
watching them go red. The parity corpus has the same defect in
`test_a_plan_that_differs_from_the_approved_one_is_refused` and
`test_a_root_with_no_approved_plan_is_refused`, whose `tofu_show` is also
empty; those recordings cannot be regenerated from this repository, so the
difference is declared as `EMPTY-PLAN-IS-NOT-GATED` in
`internal/parity/divergences.go` rather than papered over.

⚠️ **This was very likely what `truss skip` was invented for.** The CLI item
above records advancing the watermark by hand, twice, on 2026-09-08, past "a
commit that could never apply" — this failure's signature. Re-read that item
before building `skip`; the reason for it may be gone.

⚠️ **It does NOT make the records write-once.** A commit that genuinely cannot
apply still rewrites `failed/<sha>` every pass, so the retention blocker below
stands.

## Gates the docs claim and the code does not check

`gates.Protection`'s own comment says adding a field without checking it is
the failure the type exists to make visible. The same failure runs in the
other direction: `scripts/protection`'s payload SETS fields that
`CheckProtection` never reads back, so the per-pass re-read — the whole
"never remembered, always re-read" premise — passes on a repository where
they have since been changed.

| Field | Set by `scripts/protection` | Read back | What it means |
| --- | --- | --- | --- |
| `bypass_pull_request_allowances` | cleared, by omission on PUT | no | named users/teams/apps merge with no review at all |
| `allow_deletions` | `false` | no | `docs/design.md` lists it as required |
| `require_last_push_approval` | not set | no | the approver may also be the last pusher |

`bypass_pull_request_allowances` is the one that matters. It is a field on
the endpoint `forge.Protection` already calls, `wireProtection` does not
decode it, and a non-empty value makes `required_approving_review_count: 1`
and `require_code_owner_reviews: true` untrue for the named actors.
`CheckApproval` still refuses the resulting commit, so this fails closed —
but `docs/threat-model.md`'s "the applier refuses EVERYTHING and alerts" is
not what happens, and the difference between "refuses everything loudly" and
"refuses one commit for a reason that names the wrong cause" is the whole
value of that row.

Already paid for: three fields, three files, and
`TestProtectionScriptSatisfiesTheGate` already exists to keep the payload and
the gate from disagreeing. Absent must be its own case, as everywhere else.

## Rulesets: a hole after all, demonstrated 2026-09-09

⚠️ **AN EARLIER VERSION OF THIS SECTION SAID THIS WAS NOT A HOLE. IT IS, AND
THE EVIDENCE IS A PUSH TO THIS REPOSITORY'S OWN main.** The reasoning that
retired it was that rulesets are additive -- repo and org rulesets layer with
classic protection and the most restrictive rule wins -- so a bypass actor
cannot weaken what classic protection already forbids. That is true and it is
not the whole question. What it misses is the case where the requirement is
enforced by a **ruleset in the first place**, because then there is nothing in
classic protection for it to be more restrictive than.

Measured: a fast-forward push of four commits straight to `main` here, which
GitHub accepted and answered with

    remote: Bypassed rule violations for refs/heads/main:
    remote: - Changes must be made through a pull request.
    remote: - Required status check "check" is expected.

"Bypassed rule violations" is ruleset language, not classic-protection
language -- classic protection declines with a protected-branch hook error and
no push happens. So on this repository the pull-request requirement and the
required check live in a **ruleset**, and the pushing identity is a **bypass
actor** on it. Both rules were skipped and the push succeeded.

⚠️ **`forge.Protection` reads exactly one endpoint:**
`/repos/{o}/{r}/branches/{branch}/protection`. It has never read
`/rulesets` or `/rules/branches/{branch}`, and `gates.Protection` has no field
for a ruleset, an enforcement level, or a bypass actor. So the dangerous
arrangement is not exotic, it is the one in front of us: classic protection
configured and compliant, a ruleset carrying the real requirement, and named
actors permitted to skip it. `CheckProtection` returns no problems and the
applier runs, having satisfied itself about a control that is not the one
actually governing the branch.

⚠️ **The failure is quiet, which is the part that matters.** Turning classic
protection off makes the applier refuse everything and say so in every alert.
Adding a bypass actor to a ruleset changes nothing it can see.

**What closing it takes.** `GET /repos/{o}/{r}/rules/branches/{branch}` returns
the effective rules for a branch across org and repo rulesets already
flattened, which is the right first read -- but it does **not** carry
`bypass_actors`. That needs `GET /repos/{o}/{r}/rulesets?includes_parents=true`
and then each ruleset that targets the branch. A new `forge.Rulesets` reader
and a `gates.CheckRulesets`, mirroring the existing `Protection`/
`CheckProtection` pair, refusing on `enforcement != "active"` and on any
non-empty (or unreadable) `bypass_actors` -- absent must be its own case, as
everywhere else in that package. Never a relaxation of the existing gate: the
two are read together and both must pass.

⚠️ **Also still true, and now more pressing:** GitHub shipped automatic
classic-to-ruleset conversion in August 2026. On a converted repository
`GET /branches/main/protection` 404s, `forge.Protection` errors and the pass
refuses -- fails closed, correctly, but it means truss cannot run at all
against a repository whose owner accepted that migration.

**CLOSED 2026-09-09.** `internal/forge/rulesets.go` and
`internal/gates.CheckRulesets` exist now, wired into `runApplyPass` in
`cmd/truss/apply_cmd.go` beside the `Protection` read, joined into the same
refusal sentence. Verified against GitHub's REST API description (not
guessed): `rules/branches/{branch}` never carries `bypass_actors`, only
`ruleset_id` per entry, confirming the two-read shape above; `enforcement` is
`active` | `evaluate` | `disabled`; a bypass actor's `actor_type` also
includes `User` (not listed above) and `bypass_mode` also includes `exempt`
(likewise not listed above). `rules/branches/{branch}` itself documents that
it omits rules from an `evaluate` or `disabled` ruleset entirely, so a
ruleset reaching `gates.Rulesets.Applicable` at all is proof it was active
moments earlier; `CheckRulesets` refuses one whose *second* read (the
per-ruleset call, which is the only one carrying `bypass_actors`) disagrees
and no longer says `active`, treating that disagreement as a race or an
attempt to dodge the bypass-actor read rather than as "additive and inert".
A ruleset that was never active in the first place is not refused for
existing, matching the ruling above that a non-enforcing ruleset is not
automatically a hole.

⚠️ **OPERATIONAL NOTE: this can stop a live applier, and that is the point.**
If the managed repository has any bypass actor on any ruleset that applies to
`main`, truss now refuses every apply -- correctly, fail-closed, the same
class of stop `CheckProtection` already causes when classic protection is
misconfigured. `CheckRulesets`'s refusal names the ruleset, its id, and the
actor types (and bypass mode) so an operator goes straight to the GitHub UI
for that ruleset rather than re-deriving which one from a generic message.
Before turning this on against a repository nobody has audited for bypass
actors, check `GET /repos/{o}/{r}/rules/branches/main` and each ruleset it
names for a non-empty `bypass_actors` -- the gate will otherwise announce it
the hard way, by refusing the next pass.

## `data "external"` executes during the applier's own plan

`docs/threat-model.md` credits a grep for `provisioner` blocks and `external`
data sources. No such check exists in this tree — it lives in the consumer's
CI, and CI is not where it matters most.

Terraform and OpenTofu read a data source **during plan**, deferring to apply
only when an argument is unknown. So `data "external"` and `data "http"` in a
merged tree run with the applier's credentials and network position on every
pass, and the daily drift pass re-plans EVERY root, so an untouched root's
data source fires once a day forever. The digest gate is downstream of this:
the code has already run before any gate is consulted. `provisioner` is the
opposite case and is safe to catch later, since it only runs on apply.

⚠️ **And a grep would be the wrong fix.** "Gate on the field, never rendered
text" applies here exactly as it did to the flow-style YAML regex. The check
is an HCL parse of the checked-out tree (`hashicorp/hcl/v2`, walking `resource`
and `data` block labels), run before `tofu init`, in the applier's own loop as
well as CI's. `terraform-config-inspect` is the wrong library: it discards
resource bodies by design.

Deferred because it needs the consumer's CI workflow read alongside it —
three questions decide the scope: what ref that workflow checks out, whether
its `permissions:` are narrowed, and whether its existing refusal runs before
`tofu init` or after. GitHub's own guidance on `pull_request_target` is that
checked-out code must be "only ever inspected as data and never executed",
and `tofu plan` is not inspection.

## The provisioner / `data "external"` gate, and why it is not built yet

`docs/threat-model.md` credits a grep for `provisioner` blocks and `external`
data sources. No such check exists in this tree — it lives in the consumer's
CI, and CI is not where it matters most. Terraform and OpenTofu read a data
source **during plan**, deferring to apply only when an argument is unknown, so
`data "external"` and `data "http"` in a merged tree execute with the applier's
credentials on every pass, and the daily drift pass re-plans EVERY root, so an
untouched root's data source fires once a day forever. The digest gate sits
downstream: the code has already run before any gate is consulted.

⚠️ **Attempted 2026-09-09 and deliberately stopped, because every route to it
either breaks a rule or ships a gate nobody can prove.** Recorded so the next
pass does not rediscover this:

- **A regex over the source is out.** "Gate on the field, never rendered text"
  exists for precisely this, and a `provisioner` inside a comment, a string or
  a heredoc is the flow-style-YAML bug again.
- **An HCL parser is out.** `hashicorp/hcl/v2` is a dependency, and
  port-plan.md §7.1 makes every package standard library only. Hand-writing an
  HCL tokenizer that gets comments, quoting and heredocs right is a real
  parser, and one that is subtly wrong fails OPEN — worse than the gap.
- **The plan JSON's `configuration` block is the right shape but unverified.**
  `tofu show -json` is documented to expose
  `configuration.root_module.resources[].provisioners[]` as fields, which would
  make provisioners a field-level check needing no parser. ⚠️ **There is no
  `tofu` binary in the development environment, so that shape cannot be
  confirmed here**, and `countResourceChanges`'s comments show this codebase's
  standard is to verify against a real `tofu show -json` before depending on a
  shape. Do not build it from the documentation alone.

**What splits cleanly when someone has a real plan file to look at:**
`provisioner` runs only at APPLY, so catching it from the plan JSON between
plan and apply is sound and complete — that half needs no parser and no
dependency. `data "external"` and `data "http"` run at PLAN, so nothing
downstream of the plan can prevent them; catching them there refuses the apply
but does not stop the execution that already happened. Those two halves want
different mechanisms and should not be built as one gate.

## The ledger records history it cannot prove

`Put` has no precondition, no `Delete` exists, and nothing links one record
to the next. Whoever holds the write credential can roll `HEAD` backwards
(replaying commits) or forwards (silently passing over them), and the
ledger's own bytes carry no evidence of it. `docs/threat-model.md` has no row
for this. The gates are a monitor — they check before the fact. There is no
auditor.

The whole answer is small: a signed checkpoint. `{seq, prev_hash, sha}`,
chained, signed with the `note` package from `x/mod`'s sumdb (Ed25519, a few lines),
written alongside each record and published on the heartbeat that already
goes out every pass — so "who remembers the tip" is answered by machinery
that already runs. A `truss ledger verify` that re-walks the bucket and
refuses a broken chain or an unexpected `seq` is the auditor.

⚠️ **Take the chain, not the tree.** A Merkle tree earns its proof machinery
when the log is too big to re-download. This one is a few thousand rows and
by design will never be the reason to scale. Sigstore's Rekor v2 moved to
exactly this shape — signed checkpoints on plain object storage, no database.
Trillian, tiles and witness networks are all for a different size of problem.

⚠️ **The `seq` doubles as a fencing token.** "One pass at a time" is a
CronJob concurrency policy, not a fence: two passes can both read `HEAD`,
both apply, and both `Put` a record, and only OpenTofu's state lock catches
the apply half. A stale writer's chain will not join at `current+1`, which
turns a silent overwrite into a refusal.

## The create-if-absent measurement is narrower than the comment says

`internal/ledger/store.go`'s note is accurate about what was measured and
draws a conclusion one step too wide. SigV4 obliges every request to carry
`x-amz-*`, the endpoint refuses mixing header families, and so
`x-goog-if-generation-match: 0` is unreachable — **to an AWS-SigV4-signing
client**. GCS's native V4 HMAC signing (`GOOG4-HMAC-SHA256`) exists precisely
to send `x-goog-*` headers, and the generation precondition is a real
create-if-absent under it.

Not a reason to reach for one today — the checkpoint chain above gives
mutual exclusion without a storage primitive. It is a reason to correct the
comment, because "this endpoint offers no create-if-absent primitive" will be
read later as a fact about the bucket rather than about the signer.

## Features other appliers have that this one does not

**Declared ordering between roots.** `TouchedRoots` returns `credentials`,
then `platform`, then `projects/<name>` alphabetically. There are no declared
edges. Two projects where one consumes the other's output apply in the right
order by alphabetical luck. Terragrunt's `dependency` blocks and Spacelift's
stack dependencies exist for exactly this. ⚠️ **The current failure mode is
safe and that is why this is deferred**: an out-of-order apply makes a later
root's plan disagree with its digest, so it is refused rather than applied
wrong. Declared edges would let it succeed instead of refuse. Build it when a
refusal traced to ordering has actually happened; the shape is a topological
sort inside `internal/repo`, pure, no I/O.

**A dead man's switch, instead of 288 messages a day.** The pass runs every
five minutes and sends a Telegram message on every one — "nothing to apply",
288 times, so that silence is detectable. The reasoning is sound and the
mechanism is the wrong one: `compose.go`'s own comment already records why,
that "an alert channel nobody reads is where a real digest-gate refusal goes
to die". The standard answer separates the two signals. The heartbeat pings
an external monitor (Healthchecks.io, Cronitor, Dead Man's Snitch, or a
Prometheus `absent()` rule) which alerts when the ping STOPS; the chat channel
carries only refusals, drift and expiries. Liveness stays provable and the
channel becomes worth reading. ⚠️ This adds a dependency whose *absence* is
the alarm, which is the one kind of dependency that fails safe.

**Per-root change counts in the alert.** The counts are already computed.
`platform: +0/~2/-1` per root reads better than one aggregate number, and
costs the breakdown rather than a new mechanism.

**Queue depth in the heartbeat.** Nothing surfaces that the applier is N
commits behind an unresolved failure until somebody goes looking. "Report the
counter that moves" argues for it directly, and until the local CLI exists
the heartbeat is the only place it could show up.

**Plan-comment length.** Atlantis chunks its PR comment fence-aware because
GitHub truncates. A `platform` plan touching hundreds of resources is the
case; a reviewer who cannot see the whole diff is approving less than the
digest covers, which touches the gate's evidentiary basis rather than just
tidiness. The same limit applies to Telegram for a drift report naming many
roots.

**Artifact attestation on the release.** DONE. Pinning by digest inside the
reviewed diff proves the diff NAMES a digest; it does not prove that digest
came from this CI rather than being typed in. `actions/attest-build-provenance`
plus `gh attestation verify` closes it using infrastructure GitHub already
hosts and this project already trusts for merge-commit verification. ⚠️ It
does NOT belong on the plan digest, where independent re-execution is already
the stronger proof and a signature would be a second way to prove one fact.

To verify an artifact, run: `gh attestation verify <artifact> --repo <owner>/<repo>`

**`govulncheck ./...`** next to `go vet` in the pre-commit chain and in
`ci.yml`. It reports only reachable vulnerabilities, so it does not bring the
noise a scanner would.

**`log/slog`.** The pass logs with `d.logf`. Structured attributes (commit,
root, duration) make a CronJob's logs greppable across passes, which is the
state every incident in `docs/` started from.

## `ci.yml` runs `go test ./...` without `-count=1`

`release.yml` has it, `ci.yml` does not, and AGENTS.md says it is not
optional. `actions/setup-go` caches by default and that cache includes
`GOCACHE`, which is where test results live — so a PR that does not touch
`go.mod` can restore cached results. One flag.

## Checked and deliberately not wanted

Recorded so the next survey does not re-derive them.

- **OPA/Conftest, Sentinel, Checkov/Trivy/Terrascan.** Every rule this
  applier needs is a small Go function over structured plan JSON, and
  `internal/gates` is already that mechanism; a policy engine is a second
  substrate for one job. Sentinel's *soft-mandatory* level — pass unless
  overridden — is the fails-open-with-a-receipt pattern this project refuses
  by construction, and is worth naming as a class rather than a product.
- **Cost estimation gates.** Needs I/O, which `internal/gates` forbids.
- **SPIFFE/SPIRE.** `internal/secrets/kv.go` already authenticates with a
  projected ServiceAccount JWT and the publisher is audience-scoped into its
  own container. SPIRE's node attestation proves the node is what the
  platform says it is and says nothing about its current integrity, so it
  moves the attestation authority without moving the trust root that
  `docs/threat-model.md` already names.
- **in-toto layouts, full TUF role separation, SBOM generation.** One
  attested step does not need a layout; TUF's role separation solves a
  multi-party registry compromise that does not exist here. TUF's
  expiry-as-refusal discipline is already independently in the sweep.
- **Auto-reconciling drift** (Flux/Spacelift). Already refused on purpose,
  and every hardened-GitOps writeup agrees: undoing a change made
  mid-incident is its own outage.
- **Retry loops.** `internal/secrets/publish.go` already states the rule.
  A failed commit does not advance HEAD, so the five-minute cadence IS the
  retry, and it is automatic — except when the commit can never succeed,
  which is the wedge above, and is a defect rather than a missing retry.
- **Web UI, stack locking, contexts, blueprints, module registries,
  environment TTL, sync waves, health assessment, apply-before-merge.** All
  solve a multi-team or multi-repo problem this does not have, or contradict
  a stated line.
- **OpenTelemetry.** No collector here, no context to propagate across
  services, and a short-lived process is the case it serves worst.
- **A notification abstraction (`shoutrrr`, `notify`).** One transport is
  one way to do things. Revisit only if a second is actually wanted.

## Making the ledger tamper-evident: WORM first, a witness second

**Raised 2026-09-09: "what about something like Hedera HCS? It's cryptographically
secure pubsub more or less. It also creates an immutable audit log."**

The question found a real hole in the signed-hash-chain proposal above, and the
hole is worth stating before the answer. **A locally-signed chain does not
defend the boundary this project already concedes.** It stops an attacker
holding the bucket write credential; it does nothing against root on the
applier's node, because that attacker holds the signing key too and re-forges a
perfectly consistent chain over whatever they rewrote. `docs/threat-model.md`
names that node as the trust root, so the chain protects everything except the
one case the threat model says is the dangerous one.

⚠️ **The prior art agrees, and is blunt about it.** SEC 17a-4's electronic
recordkeeping rule has never accepted a hash chain on its own, because a chain
proves internal consistency and not resistance to the operator who holds the
keys. Compliance-grade systems combine storage-level immutability with an
independently-controlled copy. Two halves, not one.

### The first half is configuration, not code

A **locked** GCS bucket retention policy. From Google's own docs: *"Once you
lock a policy, you cannot remove it or reduce the retention period it has"*,
*"Locking a bucket's retention policy is an irreversible action"*, and objects
*"can only be deleted or replaced once their age is greater than the retention
period"* — enforced by Cloud Storage itself rather than by IAM, so it holds
against the credential holder, the project owner and an org admin alike.
Locking also places a project lien, so the project cannot be deleted out from
under it.

⚠️ **Object holds are NOT this and must not be mistaken for it.** A temporary
or event-based hold is released by whoever has the write credential — the same
identity the attacker has. Only a *locked retention policy* has no release
mechanism for anyone.

⚠️ **Retention forbids replace as well as delete**, so `HEAD` and `heartbeat`
cannot live under it: overwriting either returns `403 retentionPolicyNotMet`.
That forces a design change, and the change is an improvement rather than a
tax. Three options, cleanest last:

1. Two buckets — append-only locked, mutable unlocked. Protects the evidence
   but leaves `HEAD` freely rewritable.
2. Versioning under retention — every historical `HEAD` survives, undeletable.
   Turns a rewrite into something *detectable*, not something prevented.
3. **Delete the mutable pointers.** `HEAD` becomes derived from the immutable
   `applied/` prefix; `heartbeat/<timestamp>` becomes one write-once object per
   pass, liveness being the newest one, aged out by an ordinary lifecycle rule.
   Nothing is left to overwrite, so retention covers the whole ledger and the
   two-bucket split disappears.

⚠️ **Option 3's price is a `List`.** `ledger.Store` has exactly `Get` and
`Put` today, and deriving HEAD needs to enumerate a prefix. That widens the
contract the design doc deliberately keeps narrow — *"the ledger row claims
durable storage and nothing more"* — so it is a real decision, not a detail.
It is still the option that removes a mechanism instead of adding one.

The chain is still worth its ~50 stdlib lines on top, for ordering and
completeness and so `truss ledger verify` is a local computation. But with
WORM underneath, the storage is doing the work and the signature is corroboration.

⚠️ **Two research passes disagreed about whether GCS's newer PER-OBJECT
retention lock is reachable through the S3 interop API** — one says its
`x-goog-object-lock-*` headers cannot be mixed with SigV4's `x-amz-*` (the same
family collision already measured for `x-goog-if-generation-match`), the other
says the S3 XML surface supports it. **It does not matter for this decision**:
the bucket-level policy is configured once out of band with `gcloud` and
enforced server-side against every write path, so it sidesteps the dispute
entirely. If per-object locking is ever wanted, measure it the way the
conditional-write question was measured.

⚠️ **And measure the bucket-level enforcement too, before relying on it.**
That it applies to SigV4 interop writes is an architectural inference — the
interop surface is a front end onto the same objects — not a sentence anyone
found in Google's docs. A scratch bucket and a one-second retention period
answers it in five minutes. This codebase has been wrong about GCS interop
twice.

### Hedera HCS: the mechanism is real, the audit log is not what it says

Recorded in full because the idea is sound and the reasons against it are
specific rather than reflexive.

What is true: a topic's **running hash** is a genuine SHA-384 chain over
(previous hash, topic id, consensus timestamp, sequence number, message,
payer), so deletion or reordering of an earlier message is detectable. A
`submitKey` restricts writes to one key. Messages cannot be edited, and a topic
created without an `adminKey` cannot be deleted.

What the pitch glosses over, and what two counter-arguments raised the same
day correctly fix:

- **History: this was NOT a real objection, and an earlier draft of this
  section said it was.** ⚠️ **The mirror node's "60 days" is a default query
  window, not a retention limit.** Hedera's wording: the API server "adds an
  implicit timestamp range [now — 60d, now] to query the database. However, a
  user can still view data older than 60 days using a timestamp range [T1, T2]
  provided in the query." Older data is retained and retrievable; you supply
  explicit ranges of at most 60 days each. And the limit applies only to the
  Hedera-operated mirror node — "users of third-party mirror node services are
  not affected."

  For this use case it would not bite at all: an audit reads back rarely and
  already knows roughly when a checkpoint was written, so it would pass an
  explicit range regardless.

  ⚠️ **And a mirror node is not the only way back to the data.** Consensus
  nodes push record files *and* signature files to AWS S3 and GCS buckets
  readable under **requester-pays** credentials — the same buckets every mirror
  node ingests from. Anyone willing to pay their own egress can pull them.
  **Durability of the record was never the problem with HCS.**

  ⚠️ **What remains true about Block Nodes is narrower.** HIP-1081 makes
  retention a **deployment tier**: a *Full History* node (genesis onward; the
  "expected Tier 1 configuration", explicitly not mandatory), a *Partial
  History* node (may prune after a configured window), and an *Archive Server*
  role the tier taxonomy notes is **not deployable as a standard profile**. And
  a Block Node serves **blocks, not topics** — `BlockAccessService` and
  `BlockStreamSubscribeService` return raw blocks, and decoding them back to
  one topic's messages is the indexing job mirror nodes exist to do. There is
  no Go client for those APIs. None of that is an argument against HCS; it is
  an argument that **the mirror node, not the Block Node, is what you would
  actually read from**, now and after cutover.

  Status: **Beta**, release candidates only (no 1.0), Java 25 and gRPC,
  council-operated private preview since November 2025, community access from
  Q1 2026, no public endpoint and no announced GA.

  ⚠️ **Open, and worth re-checking after November 2026:** whether the
  Hedera-operated mirror node keeps the same implicit window once block streams
  become canonical and mirror operators re-point at the new buckets. Nothing
  found either way; do not assume it carries over unchanged.
- **Confidentiality: real, and there is a better answer than encryption.**
  `submitKey` is a write ACL only; topic contents are globally readable,
  permanently. Client-side encryption fixes that. ⚠️ **Submitting only the
  checkpoint HASH fixes it better**, because it removes a credential instead of
  adding one. An encryption key must outlive the audit log — lose it in year
  three and the log is gone; leak it and every past payload is readable
  forever, irrevocably, because nothing published there can be withdrawn.
  Ciphertext on a permanent public ledger is a harvest-now-decrypt-later
  surface; a hash is not. The content stays in the operator's own storage,
  which is what the WORM half is already for, and the witness proves only what
  a witness is for: that this checkpoint existed at this time, in this order.
- **Verification: possible today, much cheaper from November 2026.** ⚠️ **An
  earlier draft here said a verifier is "taking a mirror node's word". That is
  true of the REST API and false of the underlying data.** The procedure mirror
  nodes themselves run is documented and anyone can run it: download the
  signature files, verify them against each node's public key from the address
  book, check that at least 1/3 by stake signed the same record-file hash,
  download the record files and verify their hashes, follow the chain hash to
  prove no file is missing, and validate the address book back to genesis.
  Independent verification does not depend on Hedera shipping anything.

  What it depends on is *work*: that procedure has no Go implementation to
  inherit (the mirror node is Java), and it is the price of not trusting a
  REST answer. From the **November 2026** cutover (consensus node
  v0.79) every block carries "a single aggregated Threshold Signature Scheme
  (TSS) signature from the majority of the network by consensus weight" —
  verification that does not depend on whoever served you the data. ⚠️ Block
  Nodes are the plumbing; **TSS is the simplification** — one aggregate
  signature to check instead of collecting a third of the network's. ⚠️ **It is
  not live yet, even in preview**: the private-preview announcement states that
  blocks are not yet signed with HIP-1200 hinTS threshold signatures. So
  November is when verification gets cheap, not when it becomes possible.
  (Staged: v0.75 in July 2026 began wrapped record blocks; the legacy record
  buckets remain readable after cutover, with no stated retention deadline.)
- ⚠️ **The write path is untouched by any of the above, and this is what
  remains.** There is no REST submit path; `TopicMessageSubmitTransaction` is
  gRPC and protobuf. The Go SDK (`hiero-sdk-go`, Linux Foundation
  Decentralized Trust) brings ~21 modules, against a `go.mod` that is three
  lines and a `go.sum` that does not exist. This is architectural rather than
  a current limitation: Block Nodes ingest only from consensus nodes and have
  no transaction-ingress role at all, so nothing downstream will ever open a
  submit path.

  Hand-rolling it is *feasible* — `net/http` speaks HTTP/2, unary gRPC is a
  content-type and a five-byte length prefix, and protobuf encoding is
  mechanical — and that is the same move as the hand-rolled SigV4. ⚠️ **But it
  inverts §7.1's reasoning rather than following it.** SigV4 was worth owning
  *because it is frozen*: "a vendor cannot change a default underneath a signer
  we own." Hedera's wire format is mid-migration through November 2026. Owning
  a moving protocol buys the maintenance burden without the stability that made
  owning the static one correct.
- Price rose to **$0.0008 per submit in January 2026**; at one checkpoint per
  five-minute pass that is roughly $84/year. Small, but it makes an HBAR
  balance a root credential whose failure mode is **depletion** — which the
  expiry sweep does not model, because it watches dates and not balances.
- No SLA.

### HashIO and the JSON-RPC path: checked 2026-09-09, does not exist yet

Checked because if a plain HTTPS-and-JSON submit path existed, the one
remaining objection — gRPC and protobuf against a stdlib-only rule — would
disappear outright. It does not, today.

- **HashIO is real and supports `eth_sendRawTransaction`.** It is Hashgraph's
  hosted deployment of the open-source Hiero JSON-RPC Relay, presenting an
  Ethereum-style JSON-RPC API over Hedera's native transactions. ⚠️ **Its own
  documentation calls it development and testing only**, with "significantly
  restrictive rate limits", describes it as "less reliable", and directs
  production users to a commercial relay or to self-hosting the relay. ⚠️ "Less
  reliable" is a poor property for the one component whose job is to still be
  there afterwards; a commercial relay puts a paid third party in the write
  path; and self-hosting the relay reintroduces gRPC anyway — inside the relay,
  a Node.js service talking to consensus nodes, rather than inside truss.

  It cannot forge a submission, since the transaction is signed locally. It can
  drop, throttle or be down, which shows up as a missing checkpoint — visible,
  and the right failure direction. So it is not something to depend on; self-hosting it is another
  service to run, which is the notariser question again in a different costume.
- ⚠️ **There is no Consensus Service system contract on mainnet.** That is what
  would let an ordinary EVM transaction submit a topic message.
  **HIP-1208, "Consensus Service Precompiled Contract for Smart Contract
  Service", is the proposal for it, and its own front matter says
  `status: Deferred`** — an unmerged pull request opened 2025-06-02, last
  touched 2026-06-23, with no reference implementation (its text says one "will
  be required as part of making this HIP Final") and the gas cost still listed
  as an open issue. The live system contracts are exchange rate, token service,
  account service, schedule service and PRNG. There is no consensus-service
  entry.

  ⚠️ Even if it shipped, its child-transaction model means an EVM success
  receipt would confirm only that the submit was *staged*; finality would still
  be a mirror-node query. Deferred is the honest reading: do not plan around it.
- ⚠️ **HIP-478 is not this, despite its title.** "Interoperability Between
  Smart Contracts and HCS" is `category: Application`, `needs-council-approval:
  No`, last updated 2022, and proposes an **oracle network proxy** between the
  two services — a third party in the write path, which is worse than the SDK,
  not better.
- **What does work today** is deploying an ordinary contract that emits a
  32-byte hash as an event and calling it via `eth_sendRawTransaction`. An
  event log is consensus-timestamped and lands in the block stream, so it
  witnesses a checkpoint about as well as a topic message does.

⚠️ **THE TRANSPORT WAS NEVER THE PROBLEM, AND SAYING "gRPC" MADE IT SOUND LIKE
IT WAS.** JSON-RPC is an HTTPS POST with a JSON body: `net/http` and
`encoding/json` reach HashIO with nothing added. The dependency question is
about *signing* and about *what can be said*, and the two candidate paths each
fail a different half:

| | Signing key | Transport | Can it say "submit a topic message"? |
| --- | --- | --- | --- |
| Native HAPI | Ed25519 — **in the standard library** | protobuf over gRPC — **not** | yes |
| EVM over JSON-RPC | secp256k1 — **not in the standard library** | HTTPS + JSON — **yes** | no: HIP-1208 is unmerged, so only contract events |

Go's `crypto/elliptic` is P-224 through P-521; there is no secp256k1. So the
EVM path needs a curve dependency plus RLP, nonces and gas — one small module
rather than twenty-one, but not zero, and curve arithmetic is precisely the
thing not to hand-roll.

Cost is not the discriminator either way: a contract call emitting one 32-byte
event is roughly 25k-40k gas, about $0.002-0.003, against $0.0008 for a native
submit. Three or four times more, and both are noise.

⚠️ **Which inverts the intuition.** The *native* path is the one closer to
being owned outright: Ed25519 is stdlib, `net/http` speaks HTTP/2, and unary
gRPC is a content-type plus a five-byte length prefix over mechanical protobuf
encoding — the same shape as the hand-rolled SigV4. What argues against owning
it is not difficulty but §7.1's actual reasoning: SigV4 was worth owning
because it is frozen, and Hedera's wire format is mid-migration until November
2026.

**Conclusion: this changes nothing about the plan, because of the notariser.**
If the witness runs as a separate component off the applier's machine, the SDK
lives there and the dependency question never reaches truss — in which case the
supported native path is simply the right one and JSON-RPC buys nothing. HashIO
would only matter if truss submitted directly, and for that it is dev-only,
still needs a non-stdlib curve, and yields an EVM event rather than an HCS
message.

⚠️ **Worth watching: HIP-1208.** If it merges and ships, `eth_sendRawTransaction`
into the HCS precompile becomes a native topic submit over plain JSON, and the
calculus for truss submitting directly changes materially. That is the one
Hedera roadmap item that would.

### Whichever witness, it runs elsewhere

⚠️ **A notariser beside the applier witnesses nothing**, because the same root
owns it. The shape that works is a separate component, off that machine,
reading the append-only prefix and submitting checkpoints — which also keeps
truss stdlib-only, since the SDK lives in the other binary. That is a real
architecture and a real operational cost, and this document already opens with
a section about things living in too many places.

Sigstore's Rekor is the alternative worth pricing against HCS: an append-only
Merkle log with **client-verifiable inclusion and consistency proofs today**,
HTTP/JSON rather than gRPC, self-hostable. Its advantage is entirely one of
timing — after the November TSS cutover, HCS's verification story is arguably
the stronger of the two, and the comparison should be made again then rather
than settled now.

### ⚠️ Locking is blocked on a fact about this ledger, measured 2026-09-09

**Retention refuses a REPLACE, not only a delete, so it protects only a bucket
whose objects are written exactly once. Two of this ledger's writes are not.**

- `failed/<sha>` is rewritten **every pass** while a commit stays refused
  (`apply_cmd.go:632, 640, 653` all write the same key). A wedged commit
  rewrites it 288 times a day.
- `applied/<sha>` is rewritten whenever `AdvanceHead` fails after it was
  written, because the next pass redoes the same commit.

Locking today would turn the two failure modes most worth recording into a
bucket that refuses to record them — a gate that breaks precisely when it
matters, which is worse than the gap it was closing.

⚠️ **A prefix split cannot fix this; only a bucket split can.** Retention is
bucket-scoped, and `LEDGER_HEAD_KEY` is `applied/HEAD` in the real
configuration (`internal/parity/harness.go`) — the mutable pointer already
lives *inside* the append-only prefix.

**What makes the records write-once is timestamped keys** —
`failed/<sha>/<ts>`, `applied/<sha>/<ts>`. That is cheap on the read side and
was checked: **nothing reads `applied/` or `failed/`.** They are write-only
records; only `HEAD`, `heartbeat` and `digests/` are ever read back. So no
`List` is needed for this, unlike deriving HEAD.

⚠️ **But it collides with `internal/parity`,** which compares ledger objects
against the bash by key (`d.Key == "failed/sha1"` in `divergences.go`).
Changing the key shape diverges every failure scenario at once, and parity is
the mechanism validating the port. **So this waits for the cutover** — it is
not hard, it is badly timed, and doing it now would spend the corpus that
exists to prove the port is faithful.

**Plan digests stay in the mutable bucket, deliberately.** They are write-once
per (head sha, root) only until CI re-runs a job, and a re-run re-filing the
same digest would be refused by retention — CI would fail for a reason that is
not about CI. They also do not need WORM: "CI is a witness, never an
instruction" means a rewritten digest can force a refusal and can never cause
an apply. The audit trail that needs to be immutable is what the applier *did*,
which is `applied/` and `failed/`.

### The order

1. **Now, and done:** `scripts/ledger-retention` to set, inspect and lock a
   policy, with `lock` refusing unless the bucket is named back to it; and
   `check-writes`, which lists objects written more than once and is the gate
   that must come up empty before anyone locks anything.
2. **Now, and unanswered:** run
   `TestRetentionIsEnforcedAgainstSigV4Writes` (`internal/ledger`) against a
   scratch bucket. Nobody knows whether Cloud Storage enforces bucket
   retention against SigV4 interop writes; the reasoning that says yes is
   architectural and this package has been wrong twice on exactly that kind of
   reasoning. ⚠️ **If it fails, the whole WORM design is void** and the answer
   belongs in that test's comment before anything else changes.
3. **After the port cutover:** timestamp the record keys, split the records
   into their own bucket, then `set`, watch, `check-writes`, and only then
   `lock`.
4. Later: remove `HEAD` and `heartbeat` as mutable keys, accepting `List`.
5. Later: chain the records, stdlib `crypto/ed25519` and `crypto/sha256`, no
   dependency.
6. Revisit the witness. ⚠️ **Two of the three objections recorded against HCS
   did not survive checking, both corrected 2026-09-09 after the owner
   challenged them**: history is retained (the 60 days is a query window) and
   independent verification is possible today (from the requester-pays
   signature and record files, not from a REST answer). Confidentiality is
   answered by submitting a hash. **What genuinely remains is one thing: the
   write path is gRPC and protobuf, permanently, and that collides with
   port-plan.md §7.1.** The off-machine notariser resolves it by keeping the
   SDK out of truss entirely — so the decision is about whether to run and fund
   a second component, not about whether Hedera is sound.

   Waiting for November 2026 is now only worth it because TSS turns
   verification from "collect a third of the network's signatures and walk the
   address book back to genesis" into "check one aggregate signature". That is
   a large difference in how much verification code has to exist, and none of
   it has to exist before then.

⚠️ **Block Node preview access is obtainable here** — stated 2026-09-09, from a
former Hedera employee. That removes the availability objection and nothing
else: a preview node whose blocks are not yet hinTS-signed hands over the data
while still requiring you to trust its operator, which is the property a mirror
node already gives and is precisely what choosing HCS over Rekor was meant to
avoid.

The right use of that access is **measurement, not adoption**. Three of the
four unknowns above are answerable by someone with a node in front of them and
by nobody else: what full history actually costs in disk, whether a block
stream can be decoded back to one topic's messages without standing up a mirror
node, and when TSS signing genuinely lands. This repository settles questions
like these by measuring — the create-if-absent precondition and the GCS
checksum incompatibility were both settled that way, and both answers were the
opposite of what the documentation implied.

⚠️ **The dependency should also not be a relationship.** A control that exists
to be trustworthy after a compromise cannot rest on a preview programme and
knowing the right people; that has to become an ordinary, documented way to
run or reach a full-history node before anything depends on it. ⚠️ Steps 1-3 are not a substitute for this and do not foreclose
   it — 17a-4's lesson is that the two halves are complementary, and the
   storage half is the one available now.
