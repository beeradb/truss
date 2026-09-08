# Credentials: root or minted, never a third kind

A credential is **a root** when nobody can mint it — no API for it exists, so
it's made by hand, dropped in the vault, and given no expiry at all.
Everything else is **minted**: born from a root with a short life, re-minted
by the applier on schedule, and written wherever whatever's using it actually
reads it. That's why the forge identities here are GitHub Apps rather than
PATs — an App's private key never expires and mints hour-long tokens on
demand, while a PAT expires on its own and nothing can renew it via API.

Every item in the vault carries an **`expires`** field: a real date, or
`never`. The applier reads every one of them, every run.

## Rotation is the applier's job, not a calendar's

Nobody renews a token on a calendar, and nothing has to remember to. Every
minted token exists as **two overlapping generations** — the current one and
the one before it, both valid — so a new token is always in place well before
the old one dies, and no consumer has to be restarted at the moment of a swap.

The rotation module turns the plan's own timestamp into a generation number:
pure arithmetic, no provider and no clock resource in state, so the same plan
at the same date is byte-identical on any machine and can be evaluated
offline. The credentials root mints `<name>-g<n>` for the current generation
and the one before it, and the vault item always holds the current one. At a
45-day period, every token is at most 90 days old and every consumer has 45
days to pick up the new value on its own refresh.

**Why not just a clock resource?** `time_rotating` can replace one token on a
schedule, but a replaced token is dead the moment the apply runs, while CI and
every pod still holding its predecessor keep using it for hours. The
overlapping pair is what removes that window.

Nothing decides "it is time": the applier **re-plans the credentials root at
the end of every run, at the last applied commit**, and the plan is empty
until the date crosses a generation boundary. On that day it mints the next
generation, retires the one two back, repoints the items and rewrites CI's
secrets — and applying it *is* the rotation. It runs whenever the
branch-protection gate passed, even if a commit failed earlier in the pass,
because a failed apply last Tuesday is not a reason for a 45-day window to
close. It never runs at a commit the loop has not applied. A rotation failure
is a failure: recorded in the ledger, non-zero exit, alert.

The credential that applies everything is the one that most needs to rotate,
so it is minted like the rest, and only the token that mints it is made by
hand. That widens nothing: a credential that can mint any credential could
always have minted this one.

What cannot be rotated is what no API can mint. Those the applier **watches**:
anything within 30 days, or with no date recorded at all, is in every
heartbeat and every daily alert until it is renewed. The nag is daily on
purpose — a hand-made credential lapsing takes the applier down with it, and a
warning sent once is a warning sent while somebody was asleep.

## Revocation is a commit, not a dashboard

Rotation is a clock: a token lives two periods however badly its day is going.
That is fine for turnover and useless for *"this leaked this morning"*, and the
answer must not be a person deleting things in a console — that is the hand
operation the whole system exists to remove.

```hcl
revoked_generations = ["g3"]
```

The rotation module drops that generation from the map every resource
iterates, so the next apply destroys its tokens and mints nothing to replace
them. **Absence rather than a flag, deliberately**: a flag would have to be
honoured by each of the resources that iterate, and the one that forgot would
keep minting the burnt key.

Revoking the **current** generation is an outage waiting to happen —
consumers still hold it — so it fails loudly unless the epoch moves in the
same commit. The ordinary response to a leak is to burn the *previous*
generation instead, the one nothing should still be using.

## The credentials root is the one CI never plans

The applier plans and applies every root the same way — except this one,
which CI never plans at all, because CI isn't allowed to read its state. That
state *is* the tokens. So what a human reviews here is the code diff itself,
not a plan, and for a credential that costs nothing: a token resource's
permissions sit right there in the HCL, the same thing a plan would have
shown anyway. It's exempt from the digest gate for the same reason. And it's
the one root the applier re-plans at the end of every run, so a rotation is
never a change nobody signed off on.

More generally: **what a root's state contains decides who may read it.** The
other roots are forbidden, by a CI-enforced rule, from declaring any credential
resource, so their state is safe for the read-only plan tier. Every token in
the system is declared in the one root whose state is encrypted.
