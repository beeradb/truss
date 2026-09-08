# Operations

What to actually do when something needs doing. Every procedure here is a
command, not a sequence of clicks — a step described as "open the settings page
and untick the box" is a step nobody can review, repeat or automate.

## Branch protection: the only way past a stuck gate

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

⚠️ **This is the escape hatch, and it is the only one.** With
`enforce_admins` on and force pushes refused, the owner is bound by the same
rules as everyone else, and there is deliberately no override for a gate that
refuses wrongly — no flag, no forced merge, no bypass list. What exists instead
is this: turn protection off, fix the thing, turn it back on.

That trade is only acceptable because the hatch is one command and leaves a
trail. Toggling protection is recorded in the repository's audit log, and while
it is off the applier is refusing every commit and saying so in every alert, so
an "off" nobody turned back on is loud rather than silent.

**Turn it back on in the same sitting.** The window where protection is off is
a window where an unreviewed commit can reach `main` and the applier will not
apply it — work piles up behind a gate that is not actually guarding anything.

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
