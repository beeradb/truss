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

⚠️ **FOUR CORRECTIONS, FOUND BY MEASURING AGAINST REAL jq DURING IMPLEMENTATION.** The first
would have made every digest wrong:

1. **`jq -S -c` writes a trailing newline, and `sha256sum` hashes it.** Verified:
   `sha256(bytes + "\n")` matches the reference pipeline and `sha256(bytes)` does not. This
   was absent from the specification entirely; an implementer following it literally would
   have produced a digest disagreeing with every value already recorded in the bucket, and the
   failure would have surfaced in production as a refusal reading like tampering.
2. **`{"resource_changes": false}` also yields `[]`.** jq's `//` treats an explicit `false` as
   absent, so "absent or null" undersold it.
3. **A `null` element of `resource_changes` is not an error.** Indexing `null` yields `null` in
   jq, so only a non-null non-object element errors.
4. **jq's sort order for non-string `.address` values is undocumented** and was reverse-engineered:
   compare sorted-key arrays first, then values in that key order.

Two divergences were found by fuzzing and deliberately NOT fixed, each documented in the test
file with its triggering input: jq's parser accepts non-RFC-8259 numbers (leading zeros, a bare
`.5`, `NaN`) that `encoding/json` refuses, and jq has a hard exponent ceiling at exactly
999,999,999 beyond which it clamps. Neither is reachable from real `tofu show -json`, because
OpenTofu's own encoder cannot emit either shape. One WAS fixed: a lone UTF-16 high surrogate,
which `encoding/json` accepts and jq's parser refuses — a document the reference pipeline
rejects must not silently produce a digest here.

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

### 4.7 `internal/secrets` — respecified against Vault (decision 4)

**What this replaces.** The earlier version specified a `Vault` interface, a `CLI` shelling to
`op`, and a sweep over two 1Password vaults. Decision 4 supersedes it: Vault is authoritative
for credential lifetimes and `op` leaves the image. Decision 5 retires the
`service-account ratelimit` diagnostic that was the only reason `RateLimits` existed. The file
reader survives untouched; everything behind it is replaced.

```go
// Unchanged. This is what secret_field/secret_field_if_present already are.
type Dir struct{ Root string }
func (d Dir) Field(item, field string) (string, error)
func (d Dir) FieldIfPresent(item, field string) (string, bool, error)

// Replaces the Vault interface and CLI. Metadata only: it cannot return a value.
type Store interface {
    Name() string
    List(ctx context.Context) ([]string, error)
    Expiry(ctx context.Context, item string) (raw string, recorded bool, err error)
}

type KVConfig struct{ Addr, Mount, Role, JWTPath string; HTTP *http.Client }
func NewKV(cfg KVConfig) (*KV, error)

type Probe interface{ Expiry(ctx context.Context) (time.Time, bool, error) }
type CloudflareToken struct{ BaseURL, Token string; HTTP *http.Client }

type Sweep struct {
    Stores   []Store
    Probes   map[string]Probe
    WarnDays int
    Now      func() time.Time
}
func (s Sweep) Run(ctx context.Context) ([]Expiring, error)

type Expiring struct{ Name string `json:"name"`; DaysLeft *int `json:"days_left"` }
func DaysUntil(now time.Time, raw string) (days int, ok bool)
```

**`Dir` does not change.** The `vault-secrets` init container renders `$SECRETS_DIR/<item>/<field>`
and exits; `docs/decisions/vault.md` calls the file interface "the seam, backend-agnostic on
purpose", and it already survived the ESO-to-Vault change without moving. Decision 4 does not
reach it.

**The sweep reads KV v2 custom metadata over `net/http`.** Three options were weighed.

1. *Init container renders expiry as files.* Rejected: it renders a HARDCODED list of twelve
   fields, which destroys the sweep's premise — the bash's own comment is "there is no list of
   names to keep in step with the runbook; the vault is the list", and a credential nobody put
   in the manifest is exactly the one the sweep exists to catch. And `set -eu` plus `need`
   turns "no expiry recorded" — which §2.16 requires be *reported* — into a pod that will not
   start.
2. *`hashicorp/vault/api`.* Rejected on decision 1's own argument: ~40 transitive modules and
   an exception to a rule made uniform for a reason, to save ~120 lines against an API surface
   of three endpoints.
3. *A `vault` binary in the image.* Replaces a 41 MB binary with a larger one. Rejected.

⚠️ **Expiry is read from `<mount>/metadata/<item>`, never `<mount>/data/`.** That makes "reads
nothing but the expiry" a property of the URL and of the policy the applier already has
(`read, list` on `platform/metadata/*`) rather than an honour-system rule in a comment. A sweep
that issues no request under `data/` is one no credential can leak through, and it is testable
as a request-path assertion rather than as a promise.

**⚠️ BLOCKED: nothing writes `expires` into Vault.** `credentials/` mints through the
1Password provider and writes each generation's expiry as a section field there;
`providers.allow` carries no Vault provider; Vault's `platform/` KV was seeded once by
`bootstrap-vault.sh`, which writes values and no metadata. Nothing re-seeds it. This is
order-of-work step 7 of `docs/decisions/vault.md`, it lives in the platform repo, and it is
five pieces: `providers.allow` gains the Vault provider by hand and CI's mirror is rebuilt;
`credentials/versions.tf` declares it; each `onepassword_item` gains a paired
`vault_kv_secret_v2` carrying `custom_metadata.expires`; `bootstrap-vault.sh` seeds
`expires = "never"` on the hand-made roots; and `credentials/` acquires a Vault identity that
can write.

⚠️ **That last piece is an owner decision.** `credentials/` runs inside the applier pod, whose
Vault role is read-only by explicit design — "a compromised applier pod cannot rewrite the
store it reads from" — and the pod has one ServiceAccount. Granting write from inside it undoes
that claim unless the write happens under a second identity.

**⚠️ A SECOND GAP, LARGER THAN DECISION 4 STATES.** The bash sweeps `platform` AND
`recipes-runtime`. Vault has one mount holding seven items. `gcp-plan` and `github-plan-app`
live in the 1Password `platform` vault and were never seeded into Vault; the whole
`recipes-runtime` vault — `nyt-cookie`, `claude-token`, `telegram`, `registry-pull`,
`r2-publish`, `r2-uploads`, `r2-archive`, plus the minted `r2-*` and `cf-images` — is served by
ESO and has no Vault mount at all. **Cutting over as things stand deletes expiry coverage of
ten items, including every credential that actually rotates.** `Sweep.Stores` is a slice and no
mount name appears in this package, so the fix is configuration once a second mount exists —
but until then the cutover is a reduction in the alarm and must be recorded as one.

**Fail closed, and "closed" is exact.** Not `die`, and not an empty list. The sweep runs at the
end of the daily pass, after rotation and drift, so exiting there discards the drift result —
the only tamper alarm the system has — while the frequent job goes on reporting "nothing to
apply" and the channel looks alive. Closed means: `Run` RETURNS its error, `cmd/truss expiry`
sets `failure`, the heartbeat is written, the alert is sent, exit 1.

Three states are errors and not results: a login, list or metadata read that fails; a partial
sweep (findings AND an error, never findings alone); and a mount that lists successfully in
which not one item records an `expires`. The last is the prerequisite's own alarm — a store
where nothing writes expiry is indistinguishable, item by item, from a store of legitimate
gaps, and seven permanent "no expiry recorded" lines are read by nobody after the third day.
One error naming the missing write is loud, and disarms itself the moment one item carries the
field.

**Can it be built now? Built, tested and released — not wired.** Every dependency is behind an
interface and an `httptest` server. What cannot happen before step 7 is the cutover, which
would turn every daily pass red on day one. So §1's step 4 splits: `secrets`, `notify` and
`config` land and delete `curl`; `op` leaves only after step 7.

**Two facts to measure, not guess.** The applier CONTAINER does not mount the `vault-token`
projected volume today — only the init container does, and `automountServiceAccountToken` is
false — so there is no JWT to log in with; that is one line in `20-cronjob.yaml`. And whether
`path "platform/metadata/*"` authorises a LIST at the mount root.

**Behaviours preserved.** `Field` fails on a missing OR empty file, with the bash's two
distinct messages, and says "continue" rather than "start" because it is also called mid-run.
`FieldIfPresent` is for `cf-infra-admin` alone. The Cloudflare probe runs FIRST and its result
survives a mount that cannot be read. Probed items never have their `expires` read. `never` is
an opt-out. An item with no `expires`, or an unparseable one, is reported with `days_left: null`
rather than skipped. An item comfortably in date is not reported. `DaysUntil` truncates toward
zero, matching bash arithmetic, so a date twelve hours past reads 0 and not -1; a bare date and
an RFC3339 instant both parse. Mounts sweep in order and a title in two mounts is reported
twice — the bash does not dedupe.

**Deliberately gone.** The one-retry-on-rate-limit loop and `quota_detail`: they exist for a
daily account-wide 1Password allowance, Vault has none, and retrying against a Vault that is
down only makes a failing pass slower.

**Refuses to.** Fall back to a network call when a mounted file is missing. Issue any request
under `<mount>/data/`. Treat a failed login, listing or metadata read as absence. Terminate the
process. Log or return any credential value, the Vault token, or the JWT. Hardcode a mount,
role, address, item name or threshold. Report a clean bill for a mount where nothing has ever
recorded an expiry. Retry a login or listing. Cache a token beyond one `Run`. Import anything
outside the standard library.

**Named tests.**
- `TestFieldRefusesAMissingMount`
- `TestAnEmptyFieldIsAsFatalAsAMissingOne`
- `TestFieldSaysContinueNotStart`
- `TestFieldNeverFallsBackToAnyRemoteCall`
- `TestFieldIfPresentDistinguishesAbsentFromUnreadable`
- `TestAnEmptyOptionalFieldReadsAsAbsent`
- `TestNoValueAppearsInAnError`
- `TestNewKVRefusesAnEmptyAddressRoleOrMount`
- `TestExactlyOneLoginPerRun`
- `TestALoginFailureIsAnErrorNotAnEmptySweep`
- `TestListRefusesToReportAnEmptyVaultItCouldNotRead`
- `TestAMetadataReadFailureIsNeverNoExpiryRecorded`
- `TestTheSweepReadsNoSecretData`
- `TestTheClientTokenAndTheJWTNeverAppearInAnError`
- `TestTheSweepFailureIsReturnedNotFatal`
- `TestTheSweepFailureNamesTheMountAndTheCallThatFailed`
- `TestAPartialSweepReturnsBothItsFindingsAndItsError`
- `TestAMountWhereNothingRecordsAnExpiryIsAnErrorNotAListOfNulls`
- `TestOneRecordedExpiryDisarmsThatRefusal`
- `TestProbedItemsAreSkippedInTheVaultScan`
- `TestTheProbeStillReportsWhenTheMountIsUnreadable`
- `TestAProbeFailureIsNoExpiryRecordedNotAnError`
- `TestNeverIsAnOptOut`
- `TestAnAbsentExpiryIsReportedWithNullDays`
- `TestAnUnparseableDateIsReportedNotSkipped`
- `TestABareDateAndAnRFC3339InstantBothParse`
- `TestDaysUntilIsNegativeForThePast`
- `TestDaysUntilTruncatesTowardZeroLikeTheBash`
- `TestAnItemComfortablyInDateIsNotReported`
- `TestWarnDaysIsTheBoundaryInclusive`
- `TestOnlyExpiresIsEverReadFromARuntimeVault`
- `TestEachMountIsSweptInOrder`
- `TestTheSameTitleInTwoMountsIsReportedTwice`
- `TestNoMountRoleOrItemNameIsHardcoded`
- `TestSecretsImportsOnlyTheStandardLibrary`
- `TestAgainstTheRealVault` (opt-in, `TRUSS_VAULT_LIVE=1`, metadata reads only)

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

Before step 5 is deployed, both jobs run in the same cluster for a week: a shadow CronJob runs
`truss apply` with **`DRIFT_CHECK=1`** and its own **`HEARTBEAT_KEY`**, while the 5-minute job
stays on bash. Divergence is then a diff of two heartbeat objects, observed rather than argued.

⚠️ **There is no `--drift` FLAG, and this paragraph used to specify one.** `DRIFT_CHECK=1` is
how the bash selects a drift pass and truss reads the same variable, so a flag would be a second
way to say one thing. `truss apply` takes no arguments at all and exits 2 if given any.

⚠️ **And the heartbeat diff will not be byte-for-byte**, because jq pretty-prints and Go's
`encoding/json` does not — every object differs in whitespace. `internal/parity` compares ledger
objects field by field for exactly this reason; a shadow comparison must do the same rather than
`diff` two files and conclude everything changed.

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

## 7. Decisions

The questions this plan opened were put to the owner and answered on 2026-09-08. They are
recorded here as decisions, with the reasoning, because an implementer needs the *why* to
tell a faithful port from a plausible one.

1. **Dependency policy: hand-rolled SigV4, zero dependencies.** `internal/ledger` signs its
   own requests in roughly 150 lines rather than taking `aws-sdk-go-v2`. The deciding
   argument is that an SDK default flip — checksum trailers — already broke this exact bucket
   on 2026-09-06, with an error that reads like a bad key and is not one. A vendor cannot
   change a default underneath a signer we own. This makes the rule uniform: **every package
   in Truss is standard library only**, which is easier to hold than a per-package exception.

2. **An absent `merge_commit_sha` is refused.** `gates.go:99` treated it as compliant; the
   bash (`apply.sh:529`) refuses it, and the package header says absent "must never read as
   compliant". Fixed, with the test watched failing against the original line first. An audit
   of every other optional field in the package found no second instance.

3. **A stale approval alongside a fresh one is ignored.** Matches the bash. GitHub's
   `dismiss_stale_reviews` already dismisses on push and the gate separately requires an
   approval AT the head sha, so a stale entry is API noise rather than evidence. Refusing on
   it would wedge any PR that got a second push. The converse is asserted too: a stale
   approval and nothing at the head is a refusal.

4. **Vault is authoritative for credential lifetimes; the expiry sweep moves there and `op`
   leaves the image entirely.** One store for what the applier reads, one fewer credential in
   the pod, 41 MB gone.

   ⚠️ **This has a prerequisite that is not yet built.** `credentials/` mints into 1Password
   through the 1Password provider — `providers.allow` carries no Vault provider — while the
   applier reads `platform/*` from Vault, seeded once by `bootstrap-vault.sh`. Nothing
   re-seeds Vault after a mint, so at the next 45-day boundary the applier renders a stale
   credential. Vault cannot be authoritative for `expires` until something writes it there.
   **This decision and that fix are one piece of work, not two.**

5. **The 1Password-from-Go question is moot.** Decision 4 removes the CLI, so nothing needs a
   Go client and the `service-account ratelimit` diagnostic goes with it. If a rate-limit
   diagnostic is wanted later it belongs wherever the 1Password writes still happen, which is
   `credentials/`, not the applier.

6. **Truss is built in its own repository and the binary is copied in.** Truss CI attaches
   the binary and a `SHA256SUMS` to a tagged GitHub release; the platform image fetches it
   with the App token it already holds and **verifies the checksum before copying**. This
   works while the repo is private, and the version that shipped is a tag rather than a
   commit nobody recorded. Rollback is editing the tag and rebuilding.

7. **Truss becomes the single digest implementation, on both sides.** CI's
   `publish-plan-digest` and the applier both download the same released binary, so one
   implementation produces both artifacts. Today two unpinned `jq` installations on two
   machines produce a value that must agree byte-for-byte forever, and jq 1.6 canonicalises
   numbers to doubles where 1.7 does not — a drift that would surface as a refusal reading
   like tampering. CI pins a truss version.

   ⚠️ The swap happens on both sides **in one change**, and only after §5.2 proves byte
   parity. If the bytes are identical the swap is a no-op by construction; if they are not,
   the swap does not happen.

8. **The branch and the alert subject become configuration, defaulting to today's values.**
   `main` and `platform applier` stay as defaults so the emitted text and behaviour do not
   change, and the engine stops naming one deployment in three places. This is the difference
   between an engine and a copy of somebody's.

9. **Fixtures are scrubbed and pinned digests are base64.** Realistic structure, identifying
   values replaced, and the expected digest stored as base64 of the raw 32 bytes so
   `leakscan` stays fully armed over every file. No directory-level exemption: this scanner
   has already been vacuous once without anyone noticing, and a guard with a hole in it is
   the thing that failure teaches you not to build.

## 8. Still to be measured, not asked

These are facts about a running system, answerable by §5.4 against a scratch prefix. Nobody
should guess them and nobody needs to decide them.

- **The ledger endpoint's addressing style** (path versus virtual-host), the **region string**
  to sign with — nothing in `applier/`, `bootstrap/` or the manifests sets `AWS_REGION`, so
  the working production requests were signed with whatever botocore defaults to — and
  whether the endpoint honours a **create-if-absent precondition** under HMAC auth. All three
  are now answered below.
- ~~**Which jq version produced the digests currently in the bucket.**~~ Answered 2026-09-08,
  and the answer is that it does not matter: **no recorded digest gates a commit that can
  still apply.** See below.

### Measured 2026-09-08

All three are now settled, from the live signing path in the production pod:

| | |
| --- | --- |
| endpoint | `https://storage.googleapis.com` |
| addressing | **path-style** (`ForcePathStyle: True`) |
| region in the credential scope | **`us-east-1`** |
| signed headers | exactly `host;x-amz-content-sha256;x-amz-date` |

⚠️ **`us-east-1` is botocore's default, not a location.** Nothing sets `AWS_REGION` anywhere,
so every working production request was signed with that scope. Google does not care what the
region says, but the signature covers it, so it must match. Do not "fix" it to a real GCP
region.

### The create-if-absent precondition: SETTLED, and the answer is no

Measured in-cluster 2026-09-08 by `TestAgainstTheRealBucket` against a scratch prefix, which
is the only instrument that could ask: `aws s3api put-object --if-none-match "*"` puts the CLI
on a trailer-adding code path and the endpoint refuses the request before evaluating the
precondition at all, so the CLI cannot answer this question about itself.

| Attempt | Result |
| --- | --- |
| `If-None-Match: *` | **200, then 200** — accepted and ignored; the second write overwrote the first |
| `x-goog-if-generation-match: 0`, unsigned | 400 `ExcessHeaderValues` |
| `x-goog-if-generation-match: 0`, **signed** | 400 `ExcessHeaderValues` |

> `Requests cannot specify both x-amz and x-goog headers.`

SigV4 obliges every request to carry `x-amz-date` and `x-amz-content-sha256`, so the native
Google precondition can never be combined with HMAC auth here. Signing the header does not
help — the refusal is about mixing the two families, not about the signature.

⚠️ **`PutIfAbsent` was therefore DELETED rather than shipped.** It had no caller, and its name
promised an atomicity the endpoint silently declines to provide: a guard that never guards,
which is worse than no guard because it stops anyone looking for the real one. Whatever needs
mutual exclusion takes it from the layer that has it — one applier pass at a time, plus
OpenTofu's state lock. `ErrExists` went with it.

Both measurements are now **assertions** in `TestAgainstTheRealBucket`, worded to fail loudly
if Google ever starts enforcing either, since that would be worth revisiting.

⚠️ **Fixing this exposed a second thing worth keeping.** `sign` now covers **every header the
client sends**, not the fixed three. A header outside the signature is one an intermediary can
add, drop or rewrite without invalidating the request, so the server may act on something the
signature never vouched for. There is no code path left that sends an unsigned header.

That result also confirms the trailer hazard is **live today** rather than a note from
2026-09-06, which is why the fake server in that package rejects trailer headers on sight.

### The stale-digest question: SETTLED, and it is moot

Measured 2026-09-08 against the live bucket and the real repository.

The image carries **jq-1.7**. `applier/plans/` holds digests for exactly three commits, and
the applier's `applied/HEAD` is `5629d88a`, which is **also the tip of `main`** — the applier
is fully caught up. The three commits carrying digests are not on `main` at all:

| Commit | What it is |
| --- | --- |
| `d9bf2dec` | `refs/pull/1/head` |
| `e34be81c` | `refs/pull/2/head` |
| `eda5c465` | `refs/pull/12/head` and `refs/pull/14/head` |

CI plans PR head commits, so every digest in the bucket belongs to a PR head that was never
what got merged. Nothing unapplied is gated by a digest whose jq provenance is unknown, so
there is nothing to re-derive or invalidate. Decision 7 removes the question going forward by
computing the digest in Go.

⚠️ **The first version of this check was VACUOUS and said the opposite.** It ran
`git merge-base --is-ancestor <sha> <head>` and read a non-zero exit as "not an ancestor" —
but that command also exits non-zero when the object is simply **not in the clone**, which was
the actual situation for all three. It reported "digest still live" for three commits while
proving only that a bare clone does not fetch `refs/pull/*`. The corrected check asks
`git cat-file -t` first, and the conclusion inverted.

That is the third time in this project a check has passed or failed for a reason unrelated to
what it claimed to test. The rule stands: **a check is only a check if you know what its
failure would mean.**

## 9. Next evolution: a scratch image, with git in the init phase

Decided 2026-09-08, deliberately NOT built with the port. Written down because
the shape is settled and one part of it is a trap.

**The goal is `FROM scratch` for the truss image.** Measured on the live
applier image, not assumed:

| binary | linkage | scratch-compatible |
| --- | --- | --- |
| `tofu` | `not a dynamic executable` — static Go | **yes** |
| `git` | libc, `ld-linux`, libpcre2, libz, **plus 166 helper programs** in its exec-path, including `git-remote-https`, which is what actually fetches | **no** |

So git is the only thing standing between truss and a scratch base, and the
image today is `ubuntu:24.04` carrying `op`, `gh`, the aws CLI, `jq`, python
and pip — every one of which the Go port already made unnecessary (§6).

⚠️ **AN INIT CONTAINER REMOVES GIT'S NETWORK USE, NOT GIT'S BINARY.**
`rev-list`, `diff --name-only`, `ls-tree` and `checkout` are local operations
and still need the executable. Reaching scratch means the init phase
materialising EVERYTHING the pass will ask git for, so the main container asks
nothing.

That is possible, because every ref the pass touches is knowable before it
runs, and the ordering has no circularity:

1. **init 1 — truss (scratch).** `truss ledger get $LEDGER_HEAD_KEY` writes
   `last` to the shared volume. Needs the ledger credential; needs no git.
   The subcommand already exists.
2. **init 2 — a git image.** Clone, fetch, then write `meta.json` (commits,
   changed files, tree roots) and a worktree per ref under `trees/<sha>/`.
   Needs the GitHub token; needs no cloud credentials.
3. **main — truss (scratch).** Reads the filesystem. `gitDriver` becomes a
   filesystem-backed implementation — the interface and its fake already
   exist, so this is a swap rather than a redesign.

⚠️ **THE TRAP: `pr.HeadSHA` COMES FROM THE FORGE, NOT FROM GIT.** The pass
checks out the PR head discovered by the approval gate, so init 2 cannot learn
that list by asking git for the commit queue alone. It works at all only
because the gate requires web-flow merge commits, which makes each PR head the
SECOND PARENT of a merge commit on main. **init 2 must therefore materialise
each queued commit AND its parents.** Materialising only the queue looks
right, passes a smoke test, and fails on the first real pull request.

**The security property is worth more than the base image.** The git container
holds the GitHub token; the truss container holds write credentials for four
clouds; neither holds both. Today one image holds all of it.

Also considered and not chosen: replacing git with the GitHub API (the compare
and tarball endpoints, decoded with `archive/tar` and `compress/gzip` — all
stdlib). It reaches scratch too and deletes three bug classes outright, but it
rewrites how truss learns about commits, where the init-phase route only moves
git out of the image.
