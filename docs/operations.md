# Operations

What to actually do when something needs doing. Every procedure here is a
command, not a sequence of clicks — a step described as "open the settings page
and untick the box" is a step nobody can review, repeat or automate.

## Branch protection: the all-or-nothing way past a stuck gate

    export TRUSS_REPO=<owner>/<the repository the applier applies>

    scripts/protection show     # what is set right now
    scripts/protection on       # set it to what the applier requires
    scripts/protection off      # remove it

⚠️ **`TRUSS_REPO` is the repository truss APPLIES, never the one truss is
built in.** There is no default, on purpose. These settings require a status
check named `plan`, which is filed by the plan job of a managed repository —
so pointing them at truss's own source repository requires a context that
cannot exist there, and `enforce_admins` leaves no way to merge past it. The
script now refuses a required check the target has never reported, but the
variable is the thing to get right.

The applier refuses to run at all unless protection is exactly as
[docs/design.md](design.md) specifies, and it re-reads it from the API on
every pass rather than trusting that somebody set it once. So this is not a
cosmetic setting: **`off` stops the applier, and `on` starts it again.**

⚠️ **This is the escape hatch for the repository as a whole, and it is the
only one at that scope.** With `enforce_admins` on and force pushes refused,
the owner is bound by the same rules as everyone else, and there is
deliberately no way to force a merge or bypass a list of names to get one
gate to stand down while every other stays up. What exists at this scope is
this: turn protection off, fix the thing, turn it back on.

A narrower override exists too, aimed at one commit rather than the whole
repository: `truss skip <sha> --reason <text>` advances HEAD past a single
commit that the applier has already tried and refused, guarded by the ledger
credential rather than repository admin, four checks, and an alert sent
before anything is written. See [docs/threat-model.md](threat-model.md) for
what it guards against and why it can still walk past a commit gate 1 there
refused.

That trade is only acceptable because the hatch is one command and leaves a
trail. Toggling protection is recorded in the repository's audit log, and while
it is off the applier is refusing every commit and saying so in every alert, so
an "off" nobody turned back on is loud rather than silent.

**Turn it back on in the same sitting.** The window where protection is off is
a window where an unreviewed commit can reach `main` and the applier will not
apply it — work piles up behind a gate that is not actually guarding anything.

### Protecting this repository is a different job

    scripts/repo-protection show | on | off | verify

`scripts/protection` states what the applier demands of a repository it
**manages**. This repository is not one of those: it declares no OpenTofu
roots, so nothing here ever files a `plan`, and requiring one deadlocked every
merge on 2026-09-08 — with `enforce_admins` on there was no override, so not
even the fix could land.

So the two are separate scripts with separate payloads, and neither pretends
to be the other. What this one asks for is ordinary hygiene: the `check` job
must pass, the branch must be current, stale approvals are dismissed, and
history cannot be force-pushed or deleted by accident.

Two settings differ from the applier's on purpose. `enforce_admins` is **off**,
because the maintainer needs a force push to remove the AI-trailer commits
before this repository is opened, and binding admins would mean turning
protection off to do it — which is how it came to be off in the first place.
Required approvals are **zero**, because a sole maintainer cannot approve their
own pull request and a rule nobody can satisfy is a rule that gets switched off.

`verify` answers "is what is live still what the file asks for", comparing
field by field against the payload rather than a list typed out again. A
setting changed by hand shows up as a line naming it:

    repo-protection: live settings differ from scripts/repo-protection
      enforce_admins: want False, live True

### What the payload is, and why it is not written down twice

`scripts/protection` holds one copy of the required settings.
`CheckProtection` in `internal/gates` decides what is acceptable.
`TestProtectionScriptSatisfiesTheGate` feeds the script's own payload through
that gate and fails if they ever disagree, so the script cannot drift into
setting protection the applier would reject. Do not restate those values in a
third place — including in this document, which is why they are not here.

## A credential is about to expire

The daily pass sweeps every credential's `expires` field and names anything
inside the warning window — `EXPIRY_WARN_DAYS`, 30 days by default — in the
heartbeat and in the alert, every day, until it is renewed.

A **minted** credential does not need you: rotation happens on its own clock.
See [docs/credentials.md](credentials.md).

A **root** credential is hand-made because no API can mint it, so a nag is the
only mechanism there is. Renew it, set a real new `expires`, and the nag stops.
The nag is daily rather than once, deliberately: a hand-made credential lapsing
takes the applier down with it, and a warning sent once is a warning sent while
somebody was asleep.

⚠️ **`never` is available and is the wrong answer to a nag.** It silences the
watch permanently, and a credential that never expires is one nobody ever looks
at again.

## Something leaked

Revocation is a commit, not a console session. Name the generation in
`revoked_generations` and the next apply destroys every token in it. The full
procedure, including why revoking the *current* generation needs the epoch
moved in the same commit, is in [docs/credentials.md](credentials.md).

## The applier will not start

    refusing to start: no <key> in the ledger

It has no resume point and will not invent one, because every guess is either
re-applying commits or silently passing over them. Bootstrap writes that first
entry once. This is the one refusal with no tail: no heartbeat, no alert, exit
non-zero — there is nothing yet to run a pass against.

Every other refusal reaches the same tail regardless: the heartbeat is written
and the alert goes out, and on the daily pass the rotation check and expiry
sweep run whatever the queue did. **Stopping the queue is not the same as going
quiet.** If the applier is refusing commits, you will hear about it on every
pass.

## A configuration drifted

The daily pass plans every configuration, applies nothing, and names each one
that differs from git. It reconciles nothing on purpose — quietly undoing a
change somebody made mid-incident would be its own outage.

So the fix is a pull request either way: either the change was wanted, and it
belongs in git, or it was not, and reverting it is a reviewed apply like any
other.

## An alert fired and you want to know more

Every alert in [observability/alerts/truss.rules.yml](../observability/alerts/truss.rules.yml)
carries its own reasoning, and the dashboards beside it are where you look
next. Three things are worth knowing before you read a panel.

**Check the freshness tile first, every time.** Truss is a CronJob and pushes
its metrics to a Pushgateway, and a gateway serves the last thing it was given
forever. An applier that has stopped running entirely still reports
`truss_pass_success 1`. `time() - truss_pass_timestamp_seconds` is the only
expression that goes bad on its own when nothing pushes; if it is red,
**nothing else on the page is evidence of anything.**

**The pass narrates itself in logfmt with a level.** Ordinary narration is
`level=info`, a non-fatal problem is `level=warn`, and something that was
*lost* — a ledger object that could not be written — is `level=error`.

    kubectl -n <namespace> logs job/<the most recent applier job> | grep level=error

**A refusal repeats.** The queue does not advance past a commit that failed, so
a real refusal is a continuous band on the timeline dashboard rather than a
spike, and its alert stays firing rather than resolving itself. A refusal that
appears once and clears was a transient forge error.

[observability/README.md](../observability/README.md) has the wiring, the whole
metric list, and the three series that are easy to misread.

## The applier is running and nothing is being reported

Two failure modes look identical from a dashboard — "no data" — and neither is
truss's.

**The scrape job is missing `honor_labels: true`.** The gateway derives
`job="truss"` and `pass="frequent"` from the push URL's path; without that
setting Prometheus overwrites `job` with the scrape job's own name, and every
selector in the rules and dashboards matches nothing. This is the first thing
to check, because it fails silently and completely.

**`METRICS_PUSH_URL` is not set on the CronJob.** Unset means the feature is
off; the pass is unaffected and pushes nothing. A gateway that is set but
unreachable is louder — the pass logs `level=warn msg="metrics push failed"`
and carries on, because a monitoring endpoint being down must never fail a pass
that applied infrastructure correctly.
