# Plan: the local `truss` CLI you can point at a stuck applier

**Status: approved, not built.** Written 2026-09-10 against `main` at `e1c9b2f`.
Implements [work-items.md](work-items.md), "A local `truss` you can point at a
stuck applier".

This file is the whole specification. Somebody builds from it alone, so
anything left implicit here becomes their judgement call — which is the defect
this plan exists to prevent. Where a decision could have gone another way, the
alternative and the reason for rejecting it are written down beside it.

---

## 0. The two findings that shaped this plan

### 0.1 The commands already exist on `main`

`truss status`, `truss why <sha>`, `truss skip <sha>` and `truss ledger get`
are **already implemented**: `cmd/truss/status_cmd.go`, `why_cmd.go`,
`skip_cmd.go`, `ledger_cmd.go`, wired into `run.go`'s dispatch table,
documented in [threat-model.md](threat-model.md) and
[operations.md](operations.md), and covered by tests. The tree is green at
`e1c9b2f` — `go build`, `go vet` and `go test ./... -count=1` all clean, with
201 tests in `cmd/truss` alone.

The work item was implemented and never struck, so a brief written from it
describes a build that has largely happened.

⚠️ **This is a GAP-CLOSING plan, not a rebuild.** `skip` in particular carries
four guards, an announce-before-act ordering, and a write-ordering test, all
paid for in a real incident on 2026-09-08. Rebuilding those from a sketch
would lose them. **Do not rewrite these four commands. Change only what each
gap below names.**

### 0.2 ⚠️ THEY ARE ON `main` AND NOT IN THE DEPLOYED IMAGE, AND THE APPLIER IS WEDGED RIGHT NOW

Read off the running applier on 2026-09-10: **v0.1.2 lists `ledger`,
`plan-digest`, `token`, `gate`, `expiry`, `notify`, `apply`, `publish` — and
no `status`, no `why`, no `skip`.** They first appear in **v0.1.3, which is
not pinned**.

So the guarded way to advance the watermark does not exist on the box that
needs it, during an incident where it is needed. That has three consequences
this plan must carry:

1. **Nothing here reaches the incident until a release is cut and pinned.**
   Code merged to `main` is not code on the applier. See §6 step 9 — the
   release and pin are part of this work, not a follow-up somebody remembers.
2. **It explains why `truss ledger put <head-key>` is the live hazard (G7).**
   On the deployed image it is not merely the shorter path to the watermark,
   it is the *only* path. Whoever is unsticking the applier tonight is using
   it, unguarded, with nothing recorded.
3. **It raises the value of D5 above the rest of the plan.** Everything else
   here is ergonomics. D5 is the difference between an out-of-band watermark
   write being recorded and being invisible.

---

## 1. What already meets the bar — do not touch

Stated so this plan is not read as "change everything".

- **Subcommand-first is already enforced.** `run.go` requires `args[0]` to be
  a known subcommand, so `truss --json status` is refused today. This avoids
  the pattern clig.dev names directly, that "a command might have a `--foo`
  flag that only works if you put it before the subcommand" — the shape
  Docker's `-H` and Terraform's `-chdir` both have.
- **The three-way exit convention exists** — 0 fine, 1 broken, 2 could-not-tell
  — and maps exactly onto what was asked for. It is the shape GNU `grep` uses
  (0 selected, 1 none selected, 2 error) and `diff`.
- **No interactive prompts anywhere.** Already scriptable. This is the
  Terraform complaint avoided by construction: its "type yes" prompt
  "completely breaks automated pipelines", which is why `-auto-approve` had to
  exist at all.
- **Errors already say what to do next**, in the house style. `skip`'s
  refusals name the next step; `status` names `--stale-after`.
- **`skip` announces before it acts and refuses if it cannot announce.**
  `TestSkipAnnouncesBeforeItActs` and `TestSkipRefusesWhenItCannotAnnounce`
  pin both halves. This is what earns the hatch the "can never do it quietly"
  property [threat-model.md](threat-model.md) claims for the blunt one.
- **`status`'s staleness check.** It compares the heartbeat's own timestamp
  against wall-clock, so a dead applier cannot read as healthy. Keep it; it is
  the whole reason that command is not a lie.

---

## 2. The gaps, each with its evidence

### G1 — There is no help system at all

`truss --help`, `truss help`, `truss -h` and `truss status --help` all fall
through to "not a subcommand" and print the `usage` constant to **stderr**,
exit **2**. There is no per-command help and no example anywhere in the
binary.

Evidence: clig.dev — "Display help when passed `-h` or `--help` flags" and
"you should be able to add `-h` to the end of anything and it should show
help"; on content, **"Lead with examples. Users tend to use examples over
other forms of documentation, so show them first in the help page."**
12 Factor CLI Apps makes the same call: no-args, `--help`, `-h` and a `help`
subcommand should all work, and all should carry usage examples.

### G2 — No `--json` on `status` or `why`

`inventory validate --json` is the only machine-readable output in the binary.
`status` and `why` are exactly what a runbook or a monitoring wrapper would
consume.

Evidence: clig.dev, "Display output as formatted JSON if `--json` is passed".
Kravtsov puts it more strongly and this plan adopts the stronger form
deliberately: **"Text output can improve over time. JSON output is a
contract... Do not try to make one format serve both jobs."**

### G3 — `truss ledger get <missing-key>` exits 2 in total silence

`cmdLedger` returns 2 on `ErrNotFound` having written nothing to either
stream. A typo'd key and a genuinely absent object are indistinguishable, and
nothing on screen says which happened.

### G4 — Exit code 2 is applied inconsistently across commands

For the same class of problem — configuration missing — `cmdWhy` returns
**1**, `cmdLedger` returns **1**, and `cmdStatus` returns **2**. One category,
three commands, two codes. This is the `kubectl get --ignore-not-found`
shape: documented one way, observed another.

### G5 — `skip` records no actor

`internal/ledger/journal.go`'s `skippedRecord` carries `skipped`, `reason`,
`at`, and a comment explicitly refusing an actor field. The owner asked for
who ran it. See D2(b) — approved.

### G6 — `skip` does not print the new watermark

It prints `skipped <sha>: <reason>`. The requirement is to print what it
skipped **and what the new watermark is**.

### G7 — ⚠️ `truss ledger put` IS AN UNGUARDED BACK DOOR TO THE WATERMARK

**This is the most important item in this plan. It must not be read as a minor
one.**

`truss ledger put <LEDGER_HEAD_KEY>` writes the watermark directly. No reason.
No check that the commit ever failed. No announcement. No record that a human
did it, or why, or when.

Every guard on `skip` is therefore **optional**, because the unguarded path is
one subcommand away and is *the exact operation performed by hand during the
2026-09-08 incident*. `skip` exists to make those writes recorded rather than
invisible. It cannot do that while `ledger put` performs the same write in
silence.

⚠️ **And on the deployed image (§0.2) it is not the shorter path — it is the
only path**, because `skip` is not in v0.1.2 at all.

D5 closes it.

### G8 — A laptop cannot run these commands without the applier's whole environment

`config.Load` demands ten variables — `REPO`, `APPROVER`, `LEDGER_BUCKET`,
`LEDGER_APPLIED_PREFIX`, `LEDGER_FAILED_PREFIX`, `LEDGER_HEAD_KEY`,
`HEARTBEAT_KEY`, `PLAN_DIGEST_PREFIX`, `WORKDIR`, `OP_TOKEN_FILE` — plus a
`SECRETS_DIR` tree of mounted credential files. A ledger read needs none of
`REPO`, `APPROVER`, `WORKDIR` or `OP_TOKEN_FILE`.

`ledger_cmd.go` already concedes the seam in a comment: *"internal/config has
no partial-load path."*

⚠️ **This is the gap most likely to defeat the whole feature.** Faced with
exporting ten variables, four of them irrelevant, against messages reading
"refusing to **start**: APPROVER is unset" on a laptop where nothing is
starting, a person writes the boto3 script instead — which is the failure the
work item exists to end.

### G9 — `status` does not name the commit it is stuck on

The sketch asks for "what commit the applier is on, what HEAD is, the gap
between them, and if it is refusing something, WHY". Today `status` prints the
watermark and the heartbeat fields only.

The "in words rather than two hashes" half is largely met already — the
heartbeat's `failure` string is a sentence, e.g. *"the plan for platform does
not match the one approved at ...: the world moved between review and
apply"*. What is missing is the `failed/<sha>` record for the commit it is
wedged on, which is one ledger read away.

---

## 3. Decisions

### D1 — Help is a separate path, never a widened one

**`truss` with no arguments keeps today's behaviour byte-for-byte**: the
concise `usage` constant to stderr, exit 2.
**`truss help`, `truss --help`, `truss -h`** print full help to **stdout**,
exit **0**.
**`truss <cmd> --help`** prints that command's help to stdout, exit 0.

Two reasons the no-args path must not change:

1. ⚠️ **`TestNoSubcommandPrintsASecret` pins it.** It asserts stderr is
   *exactly* the `usage` constant, stdout is empty, and exit is 2, across five
   argv shapes including one where the subcommand slot itself carries a
   secret-shaped value. Its own comment records that making that path
   interpolate argv — a plausible "helpful" change, `unknown subcommand %q` —
   was tried and watched fail. **Help work that touches this path re-opens a
   hole somebody already closed.**
2. clig.dev endorses concise help on no-args but does not ask for exit 0, and
   treating "you asked for nothing" as success is wrong.

**The security property is preserved by dispatching help only on an exact
match**: either `args[0]` is literally `help`, `--help` or `-h`, or `args[0]`
is an *already-known* subcommand and a help flag appears in its arguments.
An unknown `args[0]` never reaches a formatting path, so
`truss <secret-shaped-value> --help` still gets the fixed constant.

#### ⚠️ The trap that makes a naive implementation wrong

`truss skip <sha> --reason "-h"` — a global pre-scan for `-h` prints help
instead of performing the skip. The mirror case is worse: a scan that runs
*after* parsing lets a destructive command act when the operator asked for
help.

So help detection is **one shared helper**:

    wantsHelp(args []string, valueFlags []string) bool

It walks argv, **skips the value of any flag that takes one**, and stops at
`--`. Each command passes its own value-taking flags (`--reason`,
`--stale-after`, `--confirm`). **Help wins over everything else and is checked
before any other parsing**, so `truss skip <sha> --reason x --confirm y -h`
prints help and writes nothing.

#### Content rules

- Every subcommand's help **leads with a runnable example**, per clig.dev.
- Global help stays a table of one-liners. It does **not** dump every flag of
  every command — that is kubernetes/kubernetes#23402, where
  `kubectl port-forward --help` prints roughly 24 lines of global flags before
  saying anything about port-forward.
- Every command's help states the exit-code table from D4.
- ⚠️ **Examples must survive `scripts/leakscan`**, which refuses IP literals
  and real-looking bucket names anywhere in the tree. Use the placeholder
  shapes leakscan already permits. **Add no new leakscan exemption** — an
  exemption drops the whole matching line, and work-items.md already records
  that the older ones excuse more than they were written for.

### D2 — `skip`: `--confirm <sha>`, and a *claimed* actor

Both halves approved. Both contradict decisions previously written down with
reasons, so both rationales are recorded here in full.

#### (a) The confirmation moves onto the command line

Today it is the environment variable `TRUSS_SKIP_I_UNDERSTAND=<sha>`,
justified in `skip_cmd.go` as following `scripts/ledger-retention`'s `lock`
idiom: *"a flag is something a script can pass reflexively on every
invocation, but typing the sha into an environment variable by hand is a
deliberate act."*

**It becomes `--confirm <sha>`**, a flag whose *value* must equal the sha
being skipped.

That satisfies both concerns at once: it is a flag, so it is discoverable in
`--help`, visible on the command line where the reader is looking, and visible
in shell history — and its value is the exact sha, which is clig.dev's own
recommendation for severe actions: *"you don't just want to prompt for
confirmation here — you want to make it hard to confirm by accident. Consider
asking them to type something non-trivial such as the name of the thing
you're deleting."* It is the GitHub repo-deletion pattern.

⚠️ **The old rationale is weaker than it reads.** A script can write
`--confirm "$SHA"` exactly as easily as it can set
`TRUSS_SKIP_I_UNDERSTAND="$SHA"`. The environment variable's claimed advantage
over a flag is largely illusory, which leaves discoverability deciding — and
**a guard nobody can find gets worked around rather than obeyed.**

⚠️ **Rejected: keep the env var AND add `--force`.** Two confirmations for one
act is worse, not safer — it is more to type without being harder to do by
accident, and it violates AGENTS.md rule 8, "One way to do things, not many."

#### (b) The record gains `claimed_by`, never `by`

`journal.go` currently refuses an actor field outright: *"recording an
unverified $USER is theatre — nothing here authenticates it, and a recorded
identity nobody checked is worse than an absent one, because it invites trust
an unauthenticated string cannot earn."*

That is **right about the authentication and wrong about the conclusion**: a
break-glass record with no actor is not neutral, it is useless in exactly the
post-incident review the record exists for. The owner's requirement: *"our
schema must allow for errata and clearly marking it as a skip, with an
explanation of how. If possible the identity of who skipped should be logged
too."*

The resolution is to name the field so it cannot invite trust it has not
earned: **`claimed_by` and `claimed_from`, never `by`.** This is the repo's
own "gate on the field, never rendered text" principle applied to naming — the
caveat lives in the schema, where a reader cannot skip past it, rather than in
a comment nobody's code reads.

- Value comes from `USER`, then `LOGNAME`, **through the existing `getenv`
  seam** — no new plumbing, no `os/user`, no cgo.
- ⚠️ **If both are unset the field is `null`** and the announcement says
  "unattributed". **No invented default**, per house rule 7 (no fallbacks).
  The key is still present, so a reader cannot mistake absence for a schema
  that predates the field.
- `claimed_from` is `os.Hostname()`, `null` on error, same rule.
- **The claimed actor rides in the Telegram announcement too.** That is the
  real accountability mechanism — the alert already precedes the act, so a
  wrong name is contradicted in real time by whoever reads it. The ledger
  field is the forensic breadcrumb, not the authority.

Replace the "deliberately no `by` field" comment with the reasoning above.
A correction replaces the claim rather than narrating it.

### D3 — `--json` means stdout carries only JSON

Add `--json` to **`status`, `why` and `skip`.**

**Not to `ledger get` / `ledger put`.** Their output already *is* the object's
bytes; wrapping it would break an existing contract for no gain. **Say so in
their help text**, rather than leaving a reader to wonder whether it was an
oversight.

⚠️ **The design rule comes from a negative that Fly's own team documented**:
flyctl mixes "a stream on non-JSON messages... with the JSON stream", to the
point they advise against consuming it to monitor deployments. So:

- Under `--json`, **stdout carries exactly one JSON document and nothing
  else.** Every human line goes to stderr or is suppressed.
- **Exit codes do not change under `--json`.** The in-repo precedent is
  `inventory validate --json`, which keeps its exit 1. Asserted, not assumed.
- **Failure paths emit JSON too.** A consumer that must parse stdout on
  success and read English on failure has no contract at all.

### D4 — Fix the exit-code inconsistency, document the table, add no new code

Documented in every command's help:

| Code | Meaning |
| --- | --- |
| 0 | the thing you asked about is fine, or the answer is yes |
| 1 | it is broken, or a guard refused |
| 2 | I could not tell — bad usage, config missing, ledger unreachable, or no such record |

**The change (G4): configuration problems exit 2 everywhere.** Currently 1 in
`why` and `ledger`, 2 in `status`. A missing environment variable is "I could
not ask", never "it is broken".

⚠️ **Deliberately NOT adding a fourth code to separate "absent" from "could
not reach".** It is tempting for `ledger get`, and it is rejected: the
requirement names three categories and 0/1/2 delivers them, and `why`'s exit 2
for "the queue has not reached this commit" is an established, documented,
tested contract. **The ambiguity is answered by fixing the message instead
(G3) — the code is coarse, the message is not.**

For the record, the finer-grained alternative exists and was considered:
`systemctl is-enabled` documents a distinct exit **4** for `not-found`
(verified against `man systemctl`, Table 1, on 2026-09-10). Adopting that
shape here would churn a contract to buy a distinction nobody asked for.

### D5 — ⚠️ `ledger put` refuses to write the watermark key

**This is the load-bearing change in the plan. See G7 and §0.2.**

`truss ledger put <LEDGER_HEAD_KEY>` **refuses by name**, exit 1, writes
nothing, and its message points at `truss skip`. Every other key is
unaffected.

Without this, every guard on `skip` is decorative. The precedent for a narrow
refusal carrying its own reason is `internal/gates`: refuse the specific
thing, name it, say what to do instead.

⚠️ **The cost, stated plainly:** a genuine bootstrap or disaster-recovery
write to the head key now needs `truss skip` or a direct S3 client. That is
the right trade — `bootstrap/bootstrap.sh` is the documented path for a first
HEAD, and a recovery operator reaching for a raw client has already decided to
go around the tool, which is a different act from doing it by accident.

⚠️ **The refusal must be narrow, and there is a test for that** — see
`TestLedgerPutStillWritesEveryOtherKey`. Without it,
`TestLedgerPutRefusesTheHeadKey` passes for an implementation that refuses
everything.

### D6 — A ledger-scoped config load

Add `config.LoadLedger(getenv)`, validating only what a ledger read needs:
`LEDGER_BUCKET`, `LEDGER_APPLIED_PREFIX`, `LEDGER_FAILED_PREFIX`,
`LEDGER_HEAD_KEY`, `HEARTBEAT_KEY`, `PLAN_DIGEST_PREFIX`, and `SECRETS_DIR`
(defaulted as today).

Used by `status`, `why`, `skip` and `ledger`. **`apply`, `publish` and every
other subcommand keep `config.Load` untouched.**

Its problem strings say what to export and why, and **must not say "refusing
to start"** — nothing is starting on a laptop.

⚠️ **This is two loaders, which brushes against "one way".** The
justification: they answer different questions — "can this pass run" versus
"can I read this ledger" — and the second is the entire premise of a CLI that
works when the cluster does not. `ledger_cmd.go` already names the seam.

⚠️ **Deferred deliberately: a config file.** 12 Factor CLI Apps argues for XDG
config (`~/.config/truss`) and it would be the larger ergonomic win. Out of
scope here because it sets a new precedent for a repository whose
configuration is deliberately all-environment, and because it needs a
precedence decision — file versus environment versus flag — that deserves its
own pass. Recorded in work-items.md, not built.

### D7 — `status` gains "stuck on", and no forge call

When the heartbeat records a failure, `status` reads `failed/<last_sha>` and
prints the commit it is wedged on together with its recorded reason.

⚠️ **No forge lookup and no `git` shell-out in this pass.** Computing a real
commit gap needs either the GitHub App private key on the laptop — a second
credential, for the one command whose premise is working when other things are
broken — or a local clone. The work item's own warning is *"IT MUST NOT NEED A
CLUSTER"*, and the spirit extends to "must not need a second credential to
answer the first question".

⚠️ **So the commit GAP is NOT delivered.** See §7. The cheap follow-up, if it
is ever wanted, is an explicit `--against <dir>` reading
`git rev-list --count <watermark>..origin/main` from a local clone — roughly
forty lines plus tests, and it changes what credentials `status` implies, which
is why it is a separate decision rather than a detail.

---

## 4. Tests — named assertions, per file

Every refusal gets a test that **watches it refuse**. Each new guard is
negative-tested: break the guard, watch the test go red, restore it. A guard
nobody has watched fail is a claim, not a check.

⚠️ **Table-driven, asserting behaviour rather than implementation.**

### `cmd/truss/help_test.go` — new

- `TestHelpFlagsAllPrintToStdoutAndExitZero` — table over `help`, `--help`,
  `-h`: stdout non-empty, stderr empty, exit 0.
- `TestNoArgumentsStillPrintsUsageToStderrAndExitsTwo` — the no-args path is
  unchanged; stderr is exactly `usage`, stdout empty, exit 2.
- `TestEverySubcommandHasItsOwnHelp` — table derived from the `subcommands`
  slice; `truss <cmd> --help` exits 0 and names `<cmd>`. Fails when a
  subcommand is added without help.
- `TestEverySubcommandHelpCarriesARunnableExample` — each help text contains a
  line beginning `truss <cmd>`. The clig.dev "lead with examples" rule as a
  guard rather than a habit.
- `TestHelpIsNotPrintedForAnUnknownSubcommand` — `truss bogus --help` and a
  secret-shaped `args[0]` with `--help` both get the fixed constant on stderr,
  exit 2, and never echo argv.
- `TestHelpWinsOverAValueThatLooksLikeAHelpFlag` — ⚠️ **the D1 trap**, both
  halves in one table: `skip <sha> --reason "-h" --confirm <sha>` performs the
  skip and prints no help; `skip <sha> --reason "x" -h` prints help and writes
  nothing.
- `TestHelpStopsAtADoubleDash` — `ledger get -- -h` treats `-h` as the key.
- `TestGlobalHelpNamesEverySubcommand` — derived from the `subcommands` slice,
  never a second hand-written list.
- `TestHelpDocumentsTheExitCodes` — every command's help states the D4 table.

### `cmd/truss/status_cmd_test.go` — extend

- `TestStatusJSONIsTheOnlyThingOnStdout` — stdout parses as JSON; asserted for
  healthy, recorded-failure and stale.
- `TestStatusJSONKeepsTheSameExitCodes` — the three existing cases re-run with
  `--json`, same codes.
- `TestStatusNamesTheCommitItIsStuckOn` — heartbeat failure plus a
  `failed/<sha>` record: output names the sha and the recorded reason.
- `TestStatusConfigProblemsExitTwo` — pins the D4 rule in the one command that
  already obeys it, so it cannot regress here.

### `cmd/truss/why_cmd_test.go` — extend

- `TestWhyJSONForEachRecordShape` — table over applied, noop, skipped, failed;
  each valid JSON on stdout with a `state` field.
- `TestWhyJSONKeepsTheAbsentExitCode` — absent record exits 2 under `--json`,
  and stdout is still valid JSON.
- `TestWhyConfigProblemsExitTwo` — **the D4 change.** Watch it fail against
  today's `return 1` before fixing.

### `cmd/truss/skip_cmd_test.go` — extend and amend

- `TestSkipRefusesWithoutTheConfirmFlag` — replaces
  `TestSkipRefusesWithoutTheConfirmationEnvVar`.
- `TestSkipRefusesWhenConfirmNamesTheWrongSha` — replaces the env-var
  equivalent.
- `TestSkipRefusesWhenTheEnvironmentVariableIsSetButTheFlagIsNot` — ⚠️
  **explicitly watches the old mechanism stop working**, so the migration is
  proven rather than assumed.
- `TestSkipRecordsTheClaimedActor` — record carries `claimed_by`; the
  announcement names it.
- `TestSkipRecordsNullWhenNoActorCanBeClaimed` — `USER` and `LOGNAME` unset:
  `claimed_by` is null, announcement says unattributed, **no default is
  invented**.
- `TestSkipPrintsTheNewWatermark` — stdout names both the skipped sha and the
  new watermark (G6).
- `TestSkipJSONIsTheOnlyThingOnStdout`.
- **Retained unchanged**: the four guard tests,
  `TestSkipWritesTheRecordBeforeAdvancingHead`,
  `TestSkipAnnouncesBeforeItActs`, `TestSkipRefusesWhenItCannotAnnounce`.

### `cmd/truss/ledger_cmd_test.go` — extend

- `TestLedgerGetSaysSoWhenTheKeyIsAbsent` — G3. Exit stays 2; stderr now names
  the key. Watch it fail against today's silent return.
- `TestLedgerPutRefusesTheHeadKey` — **D5.** Exit 1, nothing written, message
  names `truss skip`. Watch it fail.
- `TestLedgerPutStillWritesEveryOtherKey` — ⚠️ **the narrowness guard.**
  Without it the previous test passes for an implementation that refuses every
  key.
- `TestLedgerConfigProblemsExitTwo` — D4. Watch it fail against today's
  `return 1`.

### `internal/ledger/journal_test.go` — extend

- `TestPutSkippedRecordsTheClaimedActor` — field order
  `skipped, reason, claimed_by, claimed_from, at`; the JSON field name is
  `claimed_by` and **never** `by`.
- `TestPutSkippedOmitsNoFieldWhenTheActorIsUnknown` — the key is present with
  a null value, so a reader cannot mistake absence for a missing schema
  version.

### `internal/config/config_test.go` — extend

- `TestLoadLedgerAcceptsAnEnvironmentWithoutTheApplierVariables` — the point
  of D6: `REPO`, `APPROVER`, `WORKDIR` and `OP_TOKEN_FILE` all unset and it
  still loads.
- `TestLoadLedgerStillRefusesAMissingBucket` — ⚠️ **the vacuous-pass guard.**
  Without it the previous test passes for a loader that validates nothing.
- `TestLoadLedgerProblemsDoNotSayRefusingToStart` — the message is right for a
  laptop.
- `TestLoadIsUnchangedForTheApplier` — `config.Load` still demands all ten.

### `cmd/truss/subcommands_test.go` — unchanged

No subcommand is added or removed. Worth noting as evidence that this change
is smaller than its description.

---

## 5. Files touched

| File | Change |
| --- | --- |
| `cmd/truss/help.go` | **new** — help texts, `wantsHelp`, help dispatch |
| `cmd/truss/help_test.go` | **new** |
| `cmd/truss/run.go` | route help; keep the no-args path byte-identical |
| `cmd/truss/status_cmd.go` | `--json`, stuck-on, exit-code fix |
| `cmd/truss/why_cmd.go` | `--json`, exit-code fix |
| `cmd/truss/skip_cmd.go` | `--confirm`, `claimed_by`, print the watermark, `--json` |
| `cmd/truss/ledger_cmd.go` | absent message, head-key refusal, exit-code fix |
| `internal/ledger/journal.go` | `skippedRecord` gains `claimed_by`/`claimed_from`; replace the "no `by` field" comment with D2(b)'s reasoning |
| `internal/config/config.go` | `LoadLedger` |
| `docs/work-items.md` | strike the item as built; record §7's non-coverage; the Vault seam note |
| `docs/threat-model.md` | the guard list names `TRUSS_SKIP_I_UNDERSTAND` |
| `docs/operations.md` | the escape-hatch description names it too |

⚠️ **Zero new module dependencies.** `go.mod` stays three lines with no
`go.sum`. Everything is standard library (`encoding/json`, `fmt`, `strings`,
`os`) plus existing internal packages.

⚠️ **One ledger client.** Everything reaches the ledger through
`buildLedgerStore` and `internal/ledger`, so the GCS checksum-header
workaround stays in the one place that already knows about it. **Do not write
a second ledger client, and do not hand-roll S3 signing.**

### The Vault seam — documented, deliberately unbuilt

Vault token minting is out of scope. The single point where Vault-minted,
short-lived ledger credentials would land is **`buildLedgerStore` in
`cmd/truss/ledger_cmd.go`** — the only constructor of a `*ledger.Store` in the
binary, already taking its endpoint and keys from `secrets.Dir`. Swapping the
source is one function body.

It is not built because it needs an identity decision nobody has made: whether
a laptop authenticates to Vault as a person (OIDC) or carries a role
credential. ⚠️ And note `internal/secrets`' `Store` interface is deliberately
**metadata-only and cannot return a secret value**, so this is not a small
wiring change — `secrets.Dir` (files on disk) is the only path that yields a
credential today.

---

## 6. Order of work

Steps 1 to 5 are independent of D2 and can proceed in any order.

1. `config.LoadLedger` plus tests. Nothing depends on it yet, so it lands
   green and alone.
2. Help system plus tests. Largest surface, zero behaviour change to existing
   commands.
3. Exit-code uniformity (D4) plus tests — each watched failing first.
4. `ledger get`'s absent message and **the head-key refusal (D5)** plus tests.
   ⚠️ If time runs short, this is the step that matters most — see §0.2.
5. `--json` on `status`, `why`, `skip` plus tests.
6. `skip`'s `--confirm` and `claimed_by` (D2), plus tests, **plus the three
   doc updates in the same commit** — `threat-model.md` and `operations.md`
   both describe the mechanism by name, and a doc naming a variable the binary
   no longer reads is worse than no doc.
7. `status` stuck-on (D7).
8. Run `scripts/check`, read it in full, then commit to `truss-cli`.
   ⚠️ **Never chained.** `scripts/check && git commit` commits either way
   under some shells, and it reads a different tree than it tested. Verify
   what will ship: `git archive HEAD` into a temp directory and run the suite
   there.
9. ⚠️ **Cut a release and pin it, or none of this reaches the applier.**
   See §0.2: the deployed image is v0.1.2 and does not contain `status`, `why`
   or `skip` at all. Merged is not deployed. This step is part of the work.

---

## 7. What this plan deliberately does NOT cover

Written down so the next person does not assume it was forgotten.

- **The commit gap in `status`.** The sketch asks for it; D7 explains why it
  needs either a second credential or a local clone, and neither belongs in a
  command that must work when everything else is broken. `--against <dir>`
  sketched, not built.
- **Vault token minting.** Out of scope by instruction; the seam is documented
  in §5.
- **A config file / XDG support.** The larger ergonomic win for laptop use,
  deferred with reasons in D6.
- **`--version`.** GNU Coding Standards require it and the binary has none.
  Out of scope here; it needs a build-stamping decision. ⚠️ Worth a
  work-items.md entry — §0.2 is a live incident where "which version is
  deployed" had to be answered by reading a usage string.
- **Colour, pagers, progress bars.** None exist and none are being added. The
  best handling of a pager is not to have one — AWS CLI v2's default pager is
  its single most-filed CLI complaint, including cases where output is lost
  entirely on quit.
- **`--` handling everywhere.** Added only where a free-form value can begin
  with `-`: `ledger` keys and `skip --reason`. POSIX asks for it generally;
  doing it across all fourteen subcommands is churn for commands that take no
  such values.
- **Rewriting the four existing commands.** See §0.1.
- **Any change to `apply`, `publish`, `gate`, `token`, `expiry`, `notify`,
  `plan-digest`, `render-digest`, `inventory` or `units`**, beyond gaining
  a help text.

---

## 8. Sources

Guidelines:

- Command Line Interface Guidelines — <https://clig.dev/>
- GNU Coding Standards, Command-Line Interfaces —
  <https://www.gnu.org/prep/standards/html_node/Command_002dLine-Interfaces.html>
- POSIX Utility Syntax Guidelines —
  <https://pubs.opengroup.org/onlinepubs/9699919799/basedefs/V1_chap12.html>
- 12 Factor CLI Apps, Jeff Dickey —
  <https://medium.com/@jdxcode/12-factor-cli-apps-dd3c227a0e46> (403s to
  automated fetch; content was retrieved via a mirror, so spot-check before
  quoting it verbatim)
- Good CLI Design Is Mostly Silence, Yar Kravtsov —
  <https://yarlson.dev/blog/good-cli-design-is-mostly-silence/>
- Heroku CLI Style Guide —
  <https://devcenter.heroku.com/articles/cli-style-guide>

Exit codes:

- GNU Grep Manual, Exit Status —
  <https://www.gnu.org/software/grep/manual/html_node/Exit-Status.html>
- systemctl(1), Table 1 —
  <https://man7.org/linux/man-pages/man1/systemctl.1.html> (verified locally
  via `man systemctl` on 2026-09-10)
- Exit Codes With Special Meanings —
  <https://tldp.org/LDP/abs/html/exitcodes.html>

Failure patterns designed against:

- kubectl help noise — <https://github.com/kubernetes/kubernetes/issues/23402>
- kubectl `--ignore-not-found` exit-code inconsistency —
  <https://github.com/kubernetes/kubectl/issues/1596>
- kubectl delete has no confirmation, with two real incidents — KEP-3895,
  <https://github.com/kubernetes/enhancements/blob/master/keps/sig-cli/3895-kubectl-delete-interactivity/README.md>
- AWS CLI v2 default pager — <https://github.com/aws/aws-cli/issues/5343>
- flyctl mixing non-JSON messages into its JSON stream —
  <https://fly.io/blog/flyctl-meets-json/>
- Terraform machine-readable output, as a positive example —
  <https://developer.hashicorp.com/terraform/internals/machine-readable-ui>
- git splitting `checkout` into `switch`/`restore` because one command did two
  jobs — <https://github.blog/open-source/git/highlights-from-git-2-23/>
