# Truss: the port plan

**Status: a specification to implement from, not a description of code.** Every claim about
current behaviour below was read out of `applier/apply.sh`, `applier/plan-digest`,
`applier/publish-plan-digest`, `applier/gh-app-token`, `applier/force-unlock`,
`applier/Dockerfile`, `applier/manifests/*.yaml`, `.github/workflows/plan.yml` and
`tests/test_infra_pipeline.py` in the reference repository, and out of
`internal/gates/gates.go` and `scripts/leakscan` here. Line numbers refer to
`applier/apply.sh` as of 2026-09-07 (1,114 lines).

The bash version keeps running throughout. Nothing in this plan requires a flag day: every
step replaces one or two shell function bodies with a call to a subcommand of one static
binary, and every step is reverted by reverting that commit and rebuilding.

## 1. The order of work, and why

`docs/decisions/engine-extraction.md` proposes the layout `cmd/applier`,
`internal/{config,forge,secrets,ledger,plan,gates,notify}`. That layout is right and is
adopted, with two changes stated in §4.6 and §4.9. What the decision record does not give is
an *order*, and the order is where the risk lives.

### The rule the order obeys

A step may land only if it is (a) callable from the running bash for one job at a time,
(b) provable against the bash before it is deployed, and (c) revertable by one commit. That
rules out "port the pass, then swap the entrypoint", which is the flag day.

### 0. Test what already exists (no deployment)

`internal/gates` is 140 lines with **no test file**. It is the only real package and nothing
has ever watched its guards fail — the standing rule in `AGENTS.md` ("a check nobody has
watched fail is a claim, not evidence") applies to it first. Writing its tests (§4.5) also
finds the two defects listed in §3 before anything is built on top of them. Half a day,
deletes nothing, unblocks the naming convention every later package copies.

### 1. `internal/ledger` — confirmed first

- **The seam is already cut.** Every ledger access goes through `AWSCLI` (line 169), which
  has exactly two callers: `ledger_put_text` (321) and `ledger_get_text` (328). Replacing
  them with `truss ledger put` / `truss ledger get` changes two function bodies.
  `ledger_put_applied`, `ledger_put_failed`, `advance_head` and `write_heartbeat` are
  untouched, so the record *shapes* do not move in this step and cannot be blamed if
  something breaks.
- **It deletes an entire language runtime, not a binary.** `python3-pip` is installed in the
  runtime stage for one line, `pip3 install awscli` (Dockerfile 106). Nothing else in the
  image is Python. 217 MB, 37% of the image, for four object operations.
- **It has no upstream dependency.** `forge`, `plan` and `notify` all consume or feed the
  ledger; the ledger consumes nothing.
- **Its blast radius is bounded and observable.** A ledger fault shows up in the heartbeat
  and the alert on the next tick, and the worst case (HEAD not advanced) is a replay of an
  applied commit, which the digest gate then refuses loudly.

The honest counter-argument: `applied/HEAD` is the resume point the design refuses to guess
(368-369), and a Go bug writing a *wrong* HEAD would skip commits silently. Answered by the
live parity test in §5.4, and by `advance_head` staying in bash for this step — the Go only
carries bytes.

**Challenged alternative: `internal/forge` first**, since `gh` and `op` are 82 MB together
and the gates are the security surface. Rejected: `forge` needs a JWT minter, a token cache,
five endpoint shapes and pointer-preserving decoding before it deletes anything, and its
failure mode is a gate that passes when it should refuse. Do the small, provable,
high-yield one first and use it to establish the test and parity conventions.

### 2. `internal/plan`, digest half only — proof, not deployment

Deletes nothing. Runs second anyway:

- The digest is the **only byte-identical requirement in the system**. Everything else can be
  "equivalent"; this must be equal, and a divergence is silent until an apply is refused as
  tampering.
- Go's `encoding/json` **does not** produce jq's bytes. Measured against `jq-1.7`: jq escapes
  `U+007F` as `` and Go does not; Go escapes `U+2028`/`U+2029` and jq does not; Go
  escapes `<`, `>`, `&` unless told otherwise and jq never does; and jq 1.7 preserves number
  *literals* (`1.0`, `2.50`, a 39-digit integer) while normalising exponent syntax
  (`1e3` → `1E+3`, `1.5e-3` → `0.0015`, `2.5e+1` → `25`, `1e21` → `1E+21`), which is
  decNumber's to-scientific-string, i.e. Java `BigDecimal.toString`.
- Nothing has to be deployed to prove it. It is a pure function with a fuzzer and a
  differential test against the real `jq` (§5.2).

Only after byte-parity is proven does `truss plan-digest` replace the bash `plan-digest` — on
**both** sides at once (applier image and CI's `publish-plan-digest`), which is safe
precisely because parity was proven first.

### 3. `internal/forge` (+ `internal/repo`'s token handling)

Deletes `gh` (41 MB) and `openssl`, and retires `applier/gh-app-token`.

The seam is the bash's own doing: `validate_pr` (506) and `verify_github_merge_commit` (545)
each print exactly one line, `OK\t…` or `FAIL\treason`, "so the caller can branch on it
without a second round of parsing" (502-505). `check_branch_protection` (457) prints a reason
and returns non-zero. Three subcommands — `truss gate protection`, `truss gate commit <sha>`,
`truss token` — drop into those three function bodies with the same protocol and exit
conventions. `GH()` then has no callers.

### 4. `internal/secrets` + `internal/notify` together

Paired because they jointly own the last two uses of `curl` (Telegram at 445, the Cloudflare
token-verify probe at 932), and because the expiry sweep is the one place `op` is still
called (957, 985). Deleting `op` (41 MB) depends on the open question in §7.1; if the answer
is "the CLI is the only thing that answers `service-account ratelimit`", `op` stays and this
step deletes only `curl`.

`internal/config` lands with this step.

### 5. `cmd/truss apply` — the pass

Last, because it is the only step that cannot be partial. Deletes `apply.sh`, `jq`, `bash`
and, if `internal/repo` keeps shelling to `git`, allows the final stage to become
`distroless/base` rather than `ubuntu:24.04`. The manifests change one line each
(`ENTRYPOINT`); both CronJobs keep their entire env block, because `internal/config` reads
exactly the same names.

### What deliberately does **not** happen

Replacing `git` (3.9 MB, 0.66% of the image). `go-git` does not implement partial clone, and
`--filter=blob:none` (line 373) is what keeps the clone cheap on a 5-minute cadence. Doing it
via the GitHub compare/tree APIs replaces a local, offline, verifiable operation with three
more network calls in the path that decides what to apply. Revisit only if the base image
becomes `scratch`.

## 2. Behaviours that must not change

Invariants, not preferences. Each is stated with where it lives in the bash so an implementer
can check the claim rather than trust it. A change to any of them is a decision for the
owner, not for an implementer.

1. **The plan digest is byte-identical.** `plan-digest` canonicalises `resource_changes`
   **only** — for each element `{address, actions: .change.actions, before: .change.before,
   after: .change.after}` — with `jq -S` (all object keys sorted, recursively), `-c`
   (compact), sorted by `.address`, then `sha256sum`, then the first field: 64 lowercase hex
   characters with no trailing newline. Nothing else in the plan JSON enters it: `timestamp`,
   `prior_state`, `configuration`, provider metadata and the input order of
   `resource_changes` all move between two runs of one plan and would produce refusals that
   look like tampering. **Any change to these bytes invalidates every digest recorded in the
   bucket**, at every head sha whose commit has not yet applied.
2. **Every gate is fail-closed, and absent is not false.** `allow_force_pushes` and
   `required_status_checks.strict` must distinguish missing from explicitly-false (467-473).
   `merge_commit_sha` absent is a refusal (529). A protection payload that cannot be read is
   a refusal (459), not a pass.
3. **A missing artifact is a refusal, never a skip.** No digest recorded for a root at the PR
   head sha → "refusing to apply a plan nobody reviewed", return 1, before any apply
   (631-651).
4. **No `tofu init` may resolve providers from a registry.** `-plugin-dir="$TF_PLUGIN_DIR"`
   and `-lockfile=readonly` on every init: the apply path (688), the drift path (1068), and
   the break-glass Job that `force-unlock` writes (`force-unlock:85`).
5. **`applied/HEAD` is never guessed.** Absent → refuse to start, naming
   `bootstrap/bootstrap.sh` (368-369).
6. **The queue stops; it never skips forward.** A failed commit files `failed/<sha>`, does not
   advance HEAD, and no later commit in the pass is attempted (820-835).
7. **A busy state lock is contention, not a fault.** Matched on OpenTofu's own "Error
   acquiring the state lock" and nothing looser (676-678); the pass ends with no
   `failed/<sha>`, no failure alert, HEAD unmoved, exit 0 (824-828, 883-887).
8. **The pass always writes a heartbeat and always sends a message**, including "nothing to
   apply", and exits 1 if and only if `failure` is non-empty (1112-1114).
9. **Root order and credential separation.** Roots are always `credentials`, then `platform`,
   then `projects/*` sorted (572-590). `credentials` runs under the mint token with
   `TF_VAR_encryption_passphrase` exported (739-743); every other root runs under
   `cf-infra-admin` with the passphrase **unset** (730-731) — including on the drift path
   (1027), which is where it leaked until 2026-09-07.
10. **Only `credentials` is exempt from the digest gate** (632), because CI never plans it.
11. **Credentials arrive as files from the mount, and there is no fallback.** A missing mount
    is fatal (136); an **empty** field is equally fatal (141); `cf-infra-admin` is the single
    credential that may legitimately be absent and has its own reader (156-159).
12. **The applying credentials are read only when the pass has work** (254-275, called at 804
    and 879), which is a budget decision, not an optimisation.
13. **Reasons are trimmed wherever they leave the pod** — ledger `failed/`, heartbeat and
    alert: NULs stripped, first 800 **bytes**, and a marker line when the original exceeded
    800 bytes (345-354, 398, 411).
14. **Rotation runs at `$LAST`, never at `origin/main`** (864), runs whenever the protection
    gate passed even if a commit failed later in the pass (1085-1105), and only on the drift
    pass.
15. **Drift reports and never applies** (`-detailed-exitcode -lock=false`, no `-out`, no
    apply: 1066-1071), drifted roots are **named, never counted** (435-438), and a drift check
    that did not run says so in the alert (425-429).
16. **The expiry sweep never reports a clean bill it did not earn.** A vault listing that
    fails sets `failure` and returns; it does not `die` (which would skip the heartbeat and
    the alert) and it does not treat an unreadable vault as an empty one (956-981).
17. **`tofu`'s output never touches the JSON channel.** `plan_and_apply`'s stdout is the
    summary JSON its caller feeds to `jq --argjson`; every tofu call redirects to stderr
    (653-663).
18. **The applier applies exactly the plan file it just made**, by one code path for all
    roots (688-716).
19. **CI's artifact is a witness, never an instruction.** Nothing published by CI is ever
    executed; the worst a compromised CI can do is force a refusal.

## 3. Deliberate divergences

Small, individually argued, each with a named test. An implementer may not add to this list.

1. **A `git rev-list` failure becomes a refusal.** `mapfile -t SHAS < <(GIT … rev-list …)`
   (760) hides a non-zero exit: the array is empty, the pass reports "nothing to apply",
   writes a healthy heartbeat and exits 0. That is the vacuous pass `AGENTS.md` has a standing
   rule against, sitting on the queue itself.
2. **"Absent" and "could not look" are distinguished at the ledger.** `ledger_get_text`
   returns 1 for a 404, a 403, a 500 and a DNS failure alike (325-332). Both still refuse —
   the behaviour does not change — but the message must name which happened.
3. **`DRIFT_CHECK` accepts only `0`, `1` or unset.** `DRIFT_CHECK=true` currently turns the
   daily job into one that runs the commit loop and checks no drift — a typo that silently
   disables the only tamper alarm.
4. **`applied/<sha>` serialises its roots in lexical order.** The bash iterates a bash
   associative array (838-840), whose order is unspecified. Nothing hashes this object;
   determinism makes it diffable.
5. **The GitHub App private key never touches disk and the installation token never enters
   argv.** The bash writes the PEM to `mktemp` (296-299) and puts the token in the clone URL
   (373). The pod is single-tenant so neither is a live exposure; in Go both are free to avoid.
6. **One digest implementation serves CI and the applier**, after §5.2 proves the bytes.

## 4. Package specifications

Conventions: Go 1.25, module `github.com/beeradb/truss`; no package writes to `os.Stdout`
except `cmd/`; no error string ever contains a credential; exported functions take
`context.Context` where they perform I/O; `internal/gates` performs no I/O and imports
nothing that does.

**A constraint every implementer will hit:** `scripts/leakscan` runs in CI over every tracked
file and refuses, case-insensitively, any email address, IP address, **32+ character hex
string**, `secret|token|password|api_key|credential` followed by `:`/`=` and 16+ characters, a
URL path into a host, or a URI into a concrete secret store or bucket. So: fixtures use non-hex fake
shas (`sha1`, `headsha1` — the reference test-suite convention), pinned digests are written as
**base64 of the raw 32 bytes** with a comment saying so, and no test fixture may name the real
repo, bucket, vault or endpoint. See §7.7.

### 4.1 `internal/config`

**Purpose.** Every knob, from the environment, validated before anything else happens.

```go
type Config struct {
    Repo, Approver                                   string
    LedgerBucket                                     string
    LedgerAppliedPrefix, LedgerFailedPrefix          string
    LedgerHeadKey, HeartbeatKey, PlanDigestPrefix    string
    Workdir, OPTokenFile                             string
    SecretsDir, PluginDir                            string // defaulted
    RequiredCheck                                    string // "plan"
    ExpiryWarnDays                                   int    // defaulted 30
    DriftOnly                                        bool
}

func Load(getenv func(string) string) (Config, []string)
```

**Behaviours preserved.** The ten required names are exactly `REPO`, `APPROVER`,
`LEDGER_BUCKET`, `LEDGER_APPLIED_PREFIX`, `LEDGER_FAILED_PREFIX`, `LEDGER_HEAD_KEY`,
`HEARTBEAT_KEY`, `PLAN_DIGEST_PREFIX`, `WORKDIR`, `OP_TOKEN_FILE` (193-201). An empty value
counts as unset. Defaults exist for exactly three: `SECRETS_DIR=/secrets` (133),
`TF_PLUGIN_DIR=/opt/tofu-providers` (115), `EXPIRY_WARN_DAYS=30` (908). `DRIFT_CHECK` is the
only boolean.

**Error semantics.** `Load` returns *every* problem, not the first; each reads
`refusing to start: $NAME is unset`. It never returns a partially populated `Config` with a
nil problem list.

**Refuses to.** Take a root list, root path, vault name, branch name or approver list from the
environment — roots are not configurable, deliberately (203-210). Carry any credential.

**Named tests.**
- `TestEveryRequiredVariableIsRefusedWhenUnset`
- `TestAnEmptyStringCountsAsUnset`
- `TestLoadReportsEveryProblemNotJustTheFirst`
- `TestOnlyThreeVariablesHaveDefaults`
- `TestDriftCheckAcceptsOnlyZeroOrOne`
- `TestRootsAreNotConfigurable`
- `TestConfigCarriesNoCredential`

### 4.2 `internal/ledger`

```go
type Config struct{ Endpoint, Bucket, Region, AccessKeyID, SecretAccessKey string }

type Store struct{ /* unexported */ }
func New(cfg Config, opts ...Option) (*Store, error)
func (s *Store) Get(ctx context.Context, key string) ([]byte, error)
func (s *Store) Put(ctx context.Context, key string, body []byte) error
func (s *Store) PutIfAbsent(ctx context.Context, key string, body []byte) error

var ErrNotFound = errors.New("ledger: key does not exist")
var ErrExists   = errors.New("ledger: key already exists")

type Layout struct{ AppliedPrefix, FailedPrefix, HeadKey, HeartbeatKey, PlanDigestPrefix string }
func (l Layout) AppliedKey(sha string) string
func (l Layout) FailedKey(sha string) string
func (l Layout) DigestKey(headSHA, root string) string // <prefix>/<sha>/<root with / -> ->.digest

type Journal struct{ Store *Store; Layout Layout; Now func() time.Time }
func (j *Journal) Head(ctx) (string, error)
func (j *Journal) AdvanceHead(ctx, sha string) error
func (j *Journal) PutApplied(ctx, sha string, roots map[string]RootSummary) error
func (j *Journal) PutNoop(ctx, sha string) error
func (j *Journal) PutFailed(ctx, sha, reason string) error
func (j *Journal) PutHeartbeat(ctx, hb Heartbeat) error
func (j *Journal) ApprovedDigest(ctx, headSHA, root string) (string, error)

type RootSummary struct{ ResourceChanges *int `json:"resource_changes"` }
type Heartbeat struct {
    Time     string          `json:"time"`
    LastSHA  string          `json:"last_sha"`
    Applied  int             `json:"applied"`
    Noop     int             `json:"noop"`
    Failure  *string         `json:"failure"`
    Rotation json.RawMessage `json:"rotation"`
    Drift    json.RawMessage `json:"drift"`
    Expiring []Expiring      `json:"expiring"`
}
func TrimReason(s string) string
```

**Behaviours preserved.** Keys: `<applied>/<sha>`, `<failed>/<sha>`, the head and heartbeat
keys verbatim, `<digestPrefix>/<headSHA>/<slug>.digest` where slug replaces **every** `/` with
`-` (637, and `publish-plan-digest:51` — the two must agree). Bodies written with no trailing
newline (320). Reads strip trailing whitespace. `{"noop":true}` exactly (773).
`failed/<sha>` is `{"reason":…,"at":…}` in that key order, `at` formatted
`2006-01-02T15:04:05Z` (358). Heartbeat key order is `time, last_sha, applied, noop, failure,
rotation, drift, expiring` (399-405), with `failure` JSON `null` when there is none.

Checksums off unless required: the request must carry **no** `x-amz-trailer`, no
`Content-Encoding: aws-chunked`, no `x-amz-checksum-*` and no
`x-amz-sdk-checksum-algorithm`. Google's S3-compatible XML API does not implement trailer
encoding and rejects such requests as `SignatureDoesNotMatch … Invalid argument`, which reads
like a bad key and is not one (measured 2026-09-06, Dockerfile 170-181, apply.sh 169-187).

**Error semantics.** `ErrNotFound` is returned **only** for a 404/`NoSuchKey`. A 403, a 5xx, a
timeout, a DNS failure or a signature error is a distinct error that must never be mistaken
for absence (§3.2). `Get` never returns `("", nil)` for a missing key.

**Refuses to.** Construct a `Store` with any of endpoint, bucket, access key or secret empty.
Expose a `Delete`. Write an untrimmed reason. Retry a `PutIfAbsent` without its precondition.

**Named tests.**
- `TestNewRefusesAnEmptyCredentialOrEndpoint`
- `TestGetReturnsErrNotFoundOnlyForAbsence`
- `TestGetOfAMissingKeyIsNeverAnEmptySuccess`
- `TestPutSendsNoChecksumTrailer`
- `TestPutIsExactBytes`
- `TestReadStripsATrailingNewline`
- `TestKeyLayoutMatchesTheBash`
- `TestDigestKeySlugReplacesEverySlash`
- `TestFailedRecordIsReasonThenAt`
- `TestFailedRecordTrimsTheReasonItself`
- `TestTrimReasonCutsAt800BytesAndSaysSo`
- `TestTrimReasonCountsBytesNotRunes`
- `TestTrimReasonDropsNULBytes`
- `TestHeartbeatFieldOrderIsStable`
- `TestAppliedRecordForANoopIsExactlyNoopTrue`
- `TestAppliedRecordSortsRootsForDeterminism`
- `TestStoreHasNoDeleteOperation`
- `TestPutIfAbsentRefusesToOverwrite`
- `TestAgainstTheRealBucket` (opt-in, `TRUSS_LEDGER_LIVE=1`, scratch prefix — §5.4)

### 4.3 `internal/plan`

Two halves in one package, separate files, separate landing steps.

#### 4.3a The digest (pure)

```go
func Canonical(planJSON []byte) ([]byte, error) // the exact bytes `jq -S -c '<filter>'` writes
func Digest(planJSON []byte) (string, error)    // 64 lowercase hex, no newline
```

`Canonical` is exported because a mismatch reported as two hashes is undiagnosable.

**The specification, exactly.** Parse preserving number literals. Take `.resource_changes`; if
absent or `null`, use `[]`. For each element build `{address, actions, before, after}` from
`.address`, `.change.actions`, `.change.before`, `.change.after`. Sort by `address` with a
**stable** sort. Emit compact JSON with **all** object keys sorted recursively by byte order.

Do not use `encoding/json`'s marshaller. Measured rules:
- Strings: escape `"` `\` and the short forms `\b \f \n \r \t`; escape every other code point
  below `0x20` **and `U+007F`** as lowercase `\u00xx`; emit everything else as raw UTF-8 —
  including `<`, `>`, `&`, `/`, `U+2028` and `U+2029`.
- Numbers: preserve significant digits and scale, normalise presentation to decNumber's
  to-scientific-string (identical to Java `BigDecimal.toString`). Verified: `1.0`→`1.0`,
  `2.50`→`2.50`, `0.1000`→`0.1000`, `-0`→`-0`, `100`→`100`, `1e3`→`1E+3`, `1E+2`→`1E+2`,
  `1.5e-3`→`0.0015`, `2.5e+1`→`25`, `1e21`→`1E+21`, and a 39-digit integer unchanged.
- `null`, `true`, `false` verbatim; no whitespace anywhere.

**Error semantics.** Malformed JSON, a `resource_changes` element that is not an object, or a
`change` present and not an object or null → error and no digest. An element with **no**
`change` key yields `actions/before/after` as `null` and is not an error.

**Refuses to.** Include any field the filter does not select. Digest a plan file. Emit
uppercase hex or a trailing newline.

**Named tests.**
- `TestDigestIgnoresWhatMovesBetweenTwoRunsOfOnePlan`
- `TestDigestChangesWhenThePlanReallyDiffers`
- `TestAnAbsentResourceChangesIsTheEmptyList`
- `TestCanonicalSortsEveryObjectKeyRecursively`
- `TestCanonicalPreservesNumberLiterals`
- `TestCanonicalNormalisesExponentsLikeDecNumber`
- `TestCanonicalEscapesExactlyWhatJQEscapes`
- `TestCanonicalKeepsSameAddressEntriesInInputOrder`
- `TestDigestRefusesMalformedJSON`
- `TestDigestRefusesANonObjectResourceChange`
- `TestDigestTreatsAMissingChangeAsNulls`
- `TestDigestIsHexLowercaseWithNoTrailingNewline`
- `TestDigestMatchesTheRecordedGoldens`
- `TestDigestAgreesWithJQ` — **fails, does not skip, when `TRUSS_REQUIRE_JQ=1`**, which CI sets
- `FuzzCanonicalAgreesWithJQ`
- `TestCanonicalNeverContainsAnUnselectedKey`

#### 4.3b The runner (exec)

```go
type Runner struct {
    Bin, PluginDir string
    Stderr         io.Writer
    Env            []string // exact environment; never inherits os.Environ implicitly
}
func (r Runner) Init(ctx, dir string) error
func (r Runner) Plan(ctx, dir, outFile string) error
func (r Runner) PlanDetailed(ctx, dir string) (changes bool, err error)
func (r Runner) Apply(ctx, dir, planFile string) error
func (r Runner) ShowJSON(ctx, dir, planFile string) ([]byte, error)
var ErrLockBusy = errors.New("plan: state lock held elsewhere")
```

**Behaviours preserved.** `init -backend-config=backend.hcl -lockfile=readonly -input=false
-plugin-dir=<dir>` (688). `plan -out=tfplan -input=false -no-color` (692). `apply -input=false
-no-color tfplan` (704). Drift: `plan -detailed-exitcode -lock=false -input=false -no-color`,
exit 0 clean, 2 changes, anything else an error (1069-1070). `ShowJSON` is `show -json <file>`
**with the file** — without it, `show` prints state, which has no `resource_changes` and
silently digests as empty (594). Every byte tofu writes goes to `r.Stderr` (653-663).

**Error semantics.** Errors carry the captured combined output. `ErrLockBusy` is returned when
and only when the output contains `Error acquiring the state lock` (676-678).

**Refuses to.** Build an `init` argv without `-plugin-dir` and `-lockfile=readonly`; build an
`apply` without a plan file; pass `-auto-approve`; inherit an ambient environment.

**Named tests.**
- `TestInitAlwaysPassesPluginDirAndReadonlyLockfile`
- `TestNoInvocationCanReachARegistry`
- `TestApplyOnlyEverAppliesAPlanFile`
- `TestPlanOutputNeverReachesStdout`
- `TestALockedStateIsContentionNotFailure`
- `TestDetailedExitcodeTwoIsDriftNotAnError`
- `TestDriftPlanNeverWrites`
- `TestShowJSONAlwaysNamesThePlanFile`
- `TestTheEnvironmentIsExactlyWhatWasGiven`

### 4.4 `internal/repo` — added to the plan of record

**Deviation, with reasons.** `engine-extraction.md` has no home for git. Putting it in `forge`
would mix an HTTP client with a process runner; putting it in `cmd` would make root derivation
untestable without a binary. `internal/repo` is also the one package that keeps a binary in
the image, so isolating it makes the eventual "delete git" decision a one-file change.

```go
type Git struct{ Bin, Dir string; Token string; Stderr io.Writer }
func (g Git) EnsureClone(ctx, repoURL string) error   // --filter=blob:none
func (g Git) Fetch(ctx, remote, branch string) error
func (g Git) Commits(ctx, from, to string) ([]string, error) // --reverse --first-parent from..to
func (g Git) ChangedFiles(ctx, sha string) ([]string, error) // diff --name-only sha^ sha
func (g Git) TreeRoots(ctx, sha string) ([]string, error)
func (g Git) Checkout(ctx, ref string) error
func (g Git) HasDir(root string) bool

func TouchedRoots(changedFiles, treeRoots []string) []string // pure
```

**Behaviours preserved (`derive_touched_roots`, 572-590).** If any changed path matches
`^modules/`, `^providers\.allow$` or `^\.opentofu-version$`, the result is `credentials` (only
if `^credentials/` also changed) followed by every `platform` / `projects/<name>` in the
commit's **tree**, and nothing else. Otherwise: `credentials` if `^credentials/` changed,
`platform` if `^platform/` changed, then each distinct `projects/<name>` from
`^projects/[^/]+/`, sorted and de-duplicated. Anchored at the start of the path. Nothing probes
the filesystem.

**Refuses to.** Put the installation token in argv (§3.5). Accept a ref that is not a full hex
sha or `origin/<branch>`. Derive roots from the working tree.

**Named tests.**
- `TestTouchedRootsOrderIsCredentialsPlatformProjectsSorted`
- `TestASharedInputPlansEveryRootInTheTree`
- `TestASharedInputAloneDoesNotIncludeCredentials`
- `TestASharedInputWithCredentialsIncludesItFirst`
- `TestRootDiscoveryDoesNotDoubleTheProjectsPrefix`
- `TestADeepFileYieldsItsProjectRootOnce`
- `TestACommitTouchingNoRootYieldsNone`
- `TestPathsThatMerelyContainARootNameAreIgnored`
- `TestCloneIsBlobless`
- `TestTheInstallationTokenIsNotInGitArgv`
- `TestCommitsIsFirstParentAndReversed`
- `TestAFailedRevListIsAnErrorNotAnEmptyQueue` (§3.1)
- `TestChangedFilesUsesTheCommitsFirstParent`

### 4.5 `internal/gates`

```go
func CheckProtection(p Protection, requiredCheck string) []string
func CheckApproval(pr PullRequest, reviews []Review, approver, sha string) []string
func CheckMergeCommit(c Commit) []string        // new
func CheckPlanDigest(root, headSHA, mine string, approved string, approvedFound bool) []string // new

type Commit struct{ SHA string; Verified *bool; CommitterLogin *string }
```

**Behaviours preserved.** The protection bar is exactly: approvals ≥ 1, code-owner reviews on,
dismiss-stale on, enforce-admins on, force-pushes **off**, `required_status_checks.strict` on,
and the required check present in **either** `contexts` or `checks[].context` (457-499). Absent
is never compliant (467-473). The approval must be `APPROVED`, by the approver, at
`pr.head.sha` (534-540). The merge commit must be GitHub's own: `verified == true` **and**
`committer.login == "web-flow"` (545-556). The digest gate exempts `credentials` and nothing
else (631-651).

**Refuses to.** Perform I/O, read a clock other than one passed in, return an empty problem
list for a zero-valued input, or accept a string where a tri-state belongs.

**Named tests.**
- `TestCheckProtectionRefusesEachMissingSetting`
- `TestAMissingKeyIsNotReadAsCompliant`
- `TestAnExplicitFalseAllowForcePushesIsCompliant`
- `TestBothStatusCheckShapesSatisfyTheGate`
- `TestEveryProtectionFieldIsChecked` (reflection: zeroing each field changes the result)
- `TestApprovalOnAnEarlierPushDoesNotCount`
- `TestApprovalByAnyoneElseDoesNotCount`
- `TestAnUnmergedPRIsRefused`
- `TestAMergeCommitThatIsNotThePRsIsRefused`
- `TestAnEmptyMergeCommitSHAIsRefused` — **currently fails**, see §7.8
- `TestAStaleApprovalAlongsideAFreshOne…` — name set by the answer to §7.9
- `TestCheckMergeCommitRequiresTheForgesOwnMerge`
- `TestCheckPlanDigestRefusesWhenNoneWasRecorded`
- `TestCheckPlanDigestRefusesAMismatchAndNamesBothValues`
- `TestCheckPlanDigestExemptsOnlyTheCredentialsRoot`
- `TestEveryGateRefusesTheZeroValue`
- `TestGatesImportsNothingThatDoesIO`

### 4.6 `internal/forge`

```go
type Config struct{ BaseURL, Repo string; AppID, InstallationID int64; PrivateKeyPEM []byte; HTTP *http.Client }
type Client struct{ /* unexported */ }
func New(cfg Config) (*Client, error)

func (c *Client) InstallationToken(ctx) (token string, expiry time.Time, err error)
func (c *Client) Protection(ctx, branch string) (gates.Protection, error)
func (c *Client) PullNumbersForCommit(ctx, sha string) ([]int, error)
func (c *Client) PullRequest(ctx, number int) (gates.PullRequest, error)
func (c *Client) Reviews(ctx, number int) ([]gates.Review, error)
func (c *Client) Commit(ctx, sha string) (gates.Commit, error)
```

**Behaviours preserved.** The App JWT is RS256 with `iat` backdated 60s and `exp` at `now+540`
(`gh-app-token:28-32`); the installation token comes from
`POST /app/installations/<id>/access_tokens`; an empty `.token` is a failure naming the API's
message. Branch protection is read once per pass, before any commit (457-459).
`commits/<sha>/pulls` yields the PR **number** only; `merged`, `merge_commit_sha` and
`head.sha` come from `pulls/<n>` (513-524).

**Refuses to.** Expose a type that could carry `merged` from the list endpoint — so the
2026-09-07 bug is a compile error rather than a comment. Default a missing protection key.
Write the private key to disk. Interpolate an unescaped repo or branch into a URL path.

**Named tests.**
- `TestPullNumbersForCommitCannotCarryMerged`
- `TestMergedComesFromTheDetailEndpoint`
- `TestProtectionKeepsAbsentAsAbsent`
- `TestProtectionReadsBothStatusCheckShapes`
- `TestANon2xxIsAnErrorNotAZeroValue`
- `TestInstallationTokenSignsAnRS256JWT`
- `TestTheKeyNeverTouchesDisk`
- `TestNoTokenAppearsInAnyError`
- `TestNewRefusesAnEmptyAppIDOrKey`
- `TestARepoNameCannotTraverseTheURLPath`
- `TestProtectionIsReadOncePerPass`

### 4.7 `internal/secrets`

```go
type Dir struct{ Root string }
func (d Dir) Field(item, field string) (string, error)
func (d Dir) FieldIfPresent(item, field string) (string, bool, error)

type Vault interface {
    ListItems(ctx, vault string) ([]string, error)
    ReadField(ctx, vault, item, field string) (string, error)
    RateLimits(ctx) string
}
type CLI struct{ Bin, Token string; Sleep func(time.Duration) }

type Sweep struct{ Vault Vault; Vaults, Probed []string; WarnDays int; Now func() time.Time }
func (s Sweep) Run(ctx, probes map[string]string) ([]Expiring, error)
type Expiring struct{ Name string `json:"name"`; DaysLeft *int `json:"days_left"` }
```

**Behaviours preserved.** `Field` fails on a missing **or** empty file, with the two distinct
messages the bash uses — "refusing to continue: … is not mounted" and "… is empty" — and
specifically not "refusing to start", because it is also called mid-run (136-142).
`FieldIfPresent` is for `cf-infra-admin` alone (145-159). The sweep lists both vaults, skips
items in `PROBED_ITEMS`, treats `expires: never` as an opt-out, and reports an item with **no**
`expires` with `days_left: null` rather than skipping it (929-990). One retry, 5 seconds, on a
rate limit, then failure (50-71).

**Error semantics.** A vault listing that fails returns an error carrying the rate-limit table
and the two-case explanation of *which* allowance ran out (976-979). The sweep never terminates
the process (956-975).

**Refuses to.** Fall back to the CLI when a mounted file is missing (129-132). Treat a failed
listing as an empty vault. Log or return any value. Read any field other than `expires` from a
runtime vault.

**Named tests.**
- `TestFieldRefusesAMissingMount`
- `TestAnEmptyFieldIsAsFatalAsAMissingOne`
- `TestFieldNeverFallsBackToTheCLI`
- `TestFieldIfPresentDistinguishesAbsentFromUnreadable`
- `TestAnEmptyOptionalFieldReadsAsAbsent`
- `TestNoValueAppearsInAnError`
- `TestListRefusesToReportAnEmptyVaultItCouldNotRead`
- `TestOneRetryOnRateLimitThenGiveUp`
- `TestTheSweepFailureNamesWhichAllowanceRanOut`
- `TestTheSweepFailureIsReturnedNotFatal`
- `TestProbedItemsAreSkippedInTheVaultScan`
- `TestNeverIsAnOptOut`
- `TestAnAbsentExpiryIsReportedWithNullDays`
- `TestAnUnparseableDateIsReportedNotSkipped`
- `TestDaysUntilIsNegativeForThePast`
- `TestOnlyExpiresIsEverReadFromARuntimeVault`

### 4.8 `internal/notify`

```go
type Report struct {
    Subject          string  // "platform applier"
    LastSHA          string
    Applied, Noop    int
    Failure          string
    DriftRun         bool
    DriftSkipped     string
    Drifted, Errored []string
    RotatedChanges   int
    Expiring         []secrets.Expiring
}
func Compose(r Report) string

type Telegram struct{ BotToken, ChatID string; HTTP *http.Client }
func (t Telegram) Send(ctx, text string) error
```

**Behaviours preserved (408-448), exactly.** Failure: `<subject> FAILED at <last>: <trimmed
reason> (applied=N noop=M)`. Idle: `<subject>: nothing to apply`. Otherwise:
`<subject>: applied=N noop=M last=<sha>`. Then, appended in this order:
`; DRIFT NOT CHECKED: <reason>` when this is a drift run and drift was skipped;
`; rotated credentials (N changes)` when N ≠ 0; `; DRIFT: a, b differ from the code`;
`; drift UNKNOWN for: a, b`; `; EXPIRING: name in 12d, other (no expiry recorded)`. Names
joined with `", "`. The body is form-encoded, so a reason containing `&` or a newline survives.

`Subject` is a field rather than a literal because this repository is the shareable engine;
`cmd/truss` defaults it so the emitted text is unchanged.

**Refuses to.** Put the bot token in an error — `net/http` embeds the request URL in
`*url.Error` and the token is *in* that URL, so every error must be rewritten, not wrapped.
Count drifted roots instead of naming them. Report a skipped drift as a clean one.

**Named tests.**
- `TestNothingToApplyMessage`
- `TestAppliedAndNoopCountsAreInTheMessage`
- `TestAFailureLeadsWithFAILEDAndTheTrimmedReason`
- `TestASkippedDriftSaysSoRatherThanLookingClean`
- `TestDriftIsNamedNeverCounted`
- `TestDriftUnknownIsDistinctFromDrift`
- `TestExpiringNamesEachCredentialAndItsDays`
- `TestRotationIsMentionedOnlyWhenSomethingChanged`
- `TestClauseOrderIsFixed`
- `TestSendFailureIsNonFatalAndSaysSo`
- `TestTheBotTokenIsNeverInAnErrorOrALog`
- `TestTextIsFormEncoded`

### 4.9 `cmd/truss` — renamed from `cmd/applier`

**Deviation, with reasons.** A flag-day-free transition requires the bash to call the Go for
one job at a time, which requires subcommands. One binary also keeps the image at one file.

| Subcommand | Replaces in the bash | Lands at step |
| --- | --- | --- |
| `ledger get <key>` / `ledger put <key>` | `ledger_get_text` / `ledger_put_text` (317-332) | 1 |
| `plan-digest` (stdin → stdout) | `applier/plan-digest` and CI's use of it | 2 |
| `token` | `applier/gh-app-token` (296-298) | 3 |
| `gate protection` | `check_branch_protection` (457-500) | 3 |
| `gate commit <sha>` | `validate_pr` + `verify_github_merge_commit` (506-556) | 3 |
| `expiry` | `check_credential_lifetimes` (929-990) | 4 |
| `notify` (report JSON on stdin) | `send_telegram` (408-448) | 4 |
| `apply` | all of `apply.sh` | 5 |

**Exit-code contract.** `ledger get`: 0 found, **2 absent**, 1 any other error — this is what
lets `apply.sh` keep `|| die` for HEAD and refuse-with-a-true-reason for a digest (§3.2).
`gate *`: 0 pass, 1 refuse with the reason on stdout, 2 could-not-ask. `apply`: 0 unless
`failure` is set, exactly as line 1114.

**Named tests.**
- `TestSubcommandsAreExactlyTheDocumentedSet`
- `TestLedgerGetExitsTwoWhenAbsentAndOneOnAnyOtherError`
- `TestPlanDigestReadsStdinAndWritesNoTrailingNewline`
- `TestGateCommitPrintsTheOneLineProtocol`
- `TestApplyRefusesToRunWithoutAValidConfig`
- `TestNoSubcommandPrintsASecret`
- `TestApplyAlwaysWritesAHeartbeatAndAlerts`
- `TestApplyExitsNonZeroIfAndOnlyIfThereIsAFailure`
- `TestLockContentionFilesNothingAndAdvancesNothing`
- `TestTheBinaryIsStatic`

## 5. Parity strategy

### 5.1 Golden corpus, captured from the bash, scrubbed once

A capture script lives in the **platform** repo (never in truss, so no real plan output can be
committed here by accident). Two corpora:

- **Digest corpus** — `tofu show -json` outputs: the synthetic ones already in
  `tests/test_infra_pipeline.py`, plus real outputs captured from one clean pass over all four
  roots, scrubbed, plus hand-written adversarial cases covering each number and escape rule in
  §4.3a. Committed to `internal/plan/testdata/digest/`. **The expected digest is stored as
  base64 of the raw 32 bytes**, because `leakscan` refuses a 64-character hex string and an
  exemption for testdata would blind the scanner to a real leak (§7.7).
- **Pass corpus** — the scenario `fixtures.json` files that `tests/test_infra_pipeline.py`
  already builds, with the repo, bucket and vault names replaced by neutral ones. The schema is
  kept **unchanged** so one corpus drives both implementations.

### 5.2 Differential test against the real tools

`TestDigestAgreesWithJQ` shells to `jq -S -c '<the exact filter>' | sha256sum | cut -d" " -f1`
for every corpus entry and every fuzz-generated document, comparing **canonical bytes first**,
then the digest. It fails rather than skips when `TRUSS_REQUIRE_JQ=1`; CI sets it.

### 5.3 Wire-shape tests instead of config assertions

For `ledger` and `forge`, the fake server **rejects** what the real endpoint rejects — a
checksum trailer, an `aws-chunked` encoding, a read of `merged` from the list route — so the
test asserts what goes on the wire. A test that asserts
`RequestChecksumCalculation == WhenRequired` passes when the SDK changes the meaning of that
constant; a server that 400s on `x-amz-trailer` does not.

### 5.4 Live, opt-in, against the real bucket and a scratch prefix

`TRUSS_LEDGER_LIVE=1 go test ./internal/ledger` writes `<prefix>/parity/<uuid>` with `truss`,
reads it back with `aws s3api get-object`, then the reverse, and byte-compares both directions
— including an empty body, a body with a NUL, and a 1 MiB body. This is the only test that
answers the region and addressing questions in §7.2.

### 5.5 Whole-pass parity, before step 5 ships

The Python harness in the reference repo drives `apply.sh` with PATH-shimmed fakes. The Go
applier makes no subprocess calls to `gh`, `aws`, `op` or `curl`, so truss ships
`internal/parity`, which reads the **same** `fixtures.json` schema and serves it over
`httptest` plus a fake `tofu` on PATH. For each scenario the harness asserts that the final
bucket contents and the alert text produced by `truss apply` equal those produced by
`apply.sh`, key by key and byte by byte, with two documented exceptions: root ordering inside
`applied/<sha>` (§3.4) and timestamps. Named: `TestPassAgreesWithBashOnEveryFixture`, one
subtest per scenario, and `TestEveryBashScenarioHasAGoSubtest`.

Before step 5 is deployed, both jobs run in the same cluster for a week: the drift CronJob runs
`truss apply --drift` writing to a **different heartbeat key**, while the 5-minute job stays on
bash. Divergence is then a diff of two heartbeat objects, observed rather than argued.

## 6. What each package deletes from the image

Measurements of the 590 MB image: Python 217 (awscli only), tofu 110, gh 41, op 41, git 3.9.

| Step | Package | Deletes | MB | How |
| --- | --- | --- | --- | --- |
| 1 | `ledger` | `awscli`, `python3-pip`, `python3` | **217** | Drop `pip3 install awscli` and `python3-pip` (Dockerfile 76-77, 106); `AWSCLI()` becomes `truss ledger`. Verify `force-unlock` first: its `python3` runs on the operator's machine. |
| 2 | `plan` (digest) | nothing | 0 | Proof step. |
| 3 | `forge` | `gh`, `openssl`, `gnupg`, one keyring/list pair | **~43** | `GH()` has exactly four call sites (459, 508, 519, 533); `gh-app-token` is retired. `force-unlock`'s Job script calls it and must change in the same commit. |
| 4 | `secrets` + `notify` | `op` (conditional on §7.1), `curl` at runtime | **41 + ~1** | `curl` has two runtime uses (445, 932); still needed at build time for tofu, so move that to the builder stage. |
| 5 | `cmd/truss` | `bash`, `jq`, coreutils, apt, the `ubuntu:24.04` base | **~78** (est.) | Final stage becomes `distroless/base` (git kept) or `scratch` (git dropped). |
| — | *not done* | `git` | 3.9 | Rejected: `--filter=blob:none` is unavailable in `go-git`; the alternative is three more API calls in the path that decides what to apply. |

**Arithmetic, stated honestly.** The five named binaries are 413 MB of 590. Of those, tofu's
110 MB stays. So the port can delete at most 303 MB of named binaries, plus an estimated 78 MB
of base image at step 5, minus whatever the final base still needs — landing near **200 MB**,
dominated by tofu plus the baked provider mirror. The ~177 MB unaccounted for is the Ubuntu
base, the provider mirror at `/opt/tofu-providers`, and `jq`/`curl`/`gnupg`/certs; **the
provider mirror is not shrinkable and must not be**, because baking it is what stops a box
holding write credentials for four clouds from reaching a package registry at apply time.
Measure the real split with `podman history` before quoting a final number.

## 7. Open questions — for the owner, not to be guessed

1. **1Password from Go.** The expiry sweep needs `item list --vault <v>`, a single field read,
   and `service-account ratelimit`. The last names *which* allowance ran out — the fix of
   2026-09-07 — and there is no evidence it is exposed anywhere but the CLI. If it is not: keep
   `op` (41 MB stays), drop the diagnostic, or move the sweep out of the applier. This decides
   step 4's headline number.
2. **The ledger endpoint.** Three things not readable from code: the addressing style
   (path vs virtual-host), the **region** string to sign with — nothing sets `AWS_REGION`, so
   production requests were signed with whatever botocore defaults to — and whether the endpoint
   honours a create-if-absent precondition under HMAC auth. The third gates `PutIfAbsent` and
   any move of `publish-plan-digest` off `gcloud`. All three are answerable by §5.4.
3. **Which jq produced the digests now in the bucket, and are CI's and the image's the same
   today?** Both are jq 1.7.x now, pinned to nothing; jq 1.6 canonicalises numbers to doubles
   and 1.7 does not. If they diverge, digests recorded by CI stop matching those computed by the
   applier — under the *bash* implementation, independent of this port. Should truss become the
   single implementation on both sides, and does CI pin a truss version?
4. **Vault or 1Password for `expires`?** The applier reads per-pass credentials from Vault-rendered
   files, while `check_credential_lifetimes` still lists two **1Password** vaults (909, 956-987).
   Which store is authoritative? If Vault, the sweep is a different function and `op` leaves the
   image at step 4 regardless of question 1.
5. **Dependency policy.** `internal/ledger` is either `aws-sdk-go-v2` (well-trodden, ~20
   transitive modules, and the checksum default that broke GCS in 2026-09-06 was an SDK default
   flip) or ~150 lines of hand-rolled SigV4 with no dependency. Leaning SDK with checksum
   settings pinned and asserted on the wire; the counter-argument — that the SDK's defaults have
   already broken this exact bucket once — is real.
6. **How truss enters the platform image, and how a step is rolled back.** A builder stage doing
   `go install …@<version>` reaches `proxy.golang.org` at build time, which the Dockerfile's
   philosophy permits but which should be said out loud. And `applier/build` imports into
   containerd without a registry — is the previous tag still on the box, or is a bad step a
   rebuild-and-redeploy?
7. **`leakscan` and testdata.** Real `tofu show -json` fixtures contain 32+ character hex ids,
   IPs, emails and bucket names; scrubbing may remove the very shapes the digest tests exercise.
   Options: synthetic fixtures only (weaker coverage), a narrow exemption for
   `internal/plan/testdata/**` (weakens a guard that has already been vacuous once), or the
   base64 convention in §5.1 plus scrubbed-but-realistic fixtures (recommended). Whichever, add a
   `scripts/leakscan-test` case for it.
8. **`internal/gates:99` — an absent `merge_commit_sha` currently passes.**
   `if pr.MergeCommitSHA != "" && pr.MergeCommitSHA != sha` treats an absent value as compliant;
   the bash (529) refuses it. This is the "absent is not false" failure, in the package written
   to prevent it. `TestAnEmptyMergeCommitSHAIsRefused` is specified on the assumption that this
   is a bug; confirm.
9. **`internal/gates:104-121` — a stale approval alongside a fresh one.** The bash counts only
   reviews at the head sha and applies; the Go appends a problem and therefore refuses. The Go
   direction is fail-closed and defensible, but it is a divergence that changes when a real merge
   applies. Which behaviour, and should the test be named `…StillRefuses` or `…IsIgnored`?
10. **Two shareable-engine warts not scoped here.** `main`/`origin/main` are hardcoded in three
    places (375, 459, 760) and the alert text begins with the literal `platform applier`
    (411-415). `Subject` is specified as a field so the emitted text is unchanged; the branch name
    is left hardcoded. Both are scope decisions for the extraction, not for the port.
