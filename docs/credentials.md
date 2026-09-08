# Credentials: root or minted, never a third kind

A credential is **a root** when nobody can mint it — no API for it exists, so
it's made by hand and dropped in the vault. Nothing can rotate it
automatically, which is exactly why it should still carry a real expiry
rather than `never`: the expiry is what stands between a credential nobody's
touched in years and one somebody's actually renewed recently. Everything
else is **minted**: born from a root with a short life, re-minted by the
applier on schedule, and written wherever whatever's using it actually
reads it. That's why the forge identities here are GitHub Apps rather than
PATs — an App's private key never expires and mints hour-long tokens on
demand, while a PAT expires on its own and nothing can renew it via API.

Every item in the vault carries an **`expires`** field: a real date, or
`never`. The applier reads every one of them, every run.

## The seven credentials

| Credential | What it is | Kind |
| --- | --- | --- |
| `cf-infra-admin` | the Cloudflare token every non-credentials root applies under | **Minted** — on the two-generation clock |
| `cf-token-mint` | the one Cloudflare token that can mint others | Root, by definition: it's the thing minting depends on |
| `github-app` private key | signs the GitHub App's identity | Root — no API mints one |
| `gcs-ledger` access/secret key | the ledger bucket's own credentials | Root |
| `gcp-apply` | the GCP service account key every root's Google provider and the ledger backend use | Root — but it needn't be, see below |
| `tofu-encryption` passphrase | encrypts the credentials root's own state | Root, and the hardest of them to change |
| `telegram-alert` bot token | where alerts go | Root |

The one that applies everything is the one that most needs to rotate, so it is
minted like anything else, and only the token that mints it is made by hand.
That widens nothing: a credential that can mint any credential could always
have minted this one.

The rest are roots because no API exists to mint them — which is the design,
not a shortfall. What it costs is that they can only be *watched*, so every one
of them carries a real expiry and gets swept on every pass.

Two of them are exceptions worth naming, because in both cases the obstacle is
effort rather than the absence of an API.

**`gcp-apply` could be minted.** GCP's IAM API creates and deletes a service
account's keys without touching what the account is allowed to do, so rotating
the key changes no IAM binding and there's none of the re-encryption problem
below. What it needs is the same minter/worker split Cloudflare already has: a
dedicated, hand-made key-minter identity holding
`iam.serviceAccountKeys.admin` on the `gcp-apply` service account and separate
from it, so the credential being rotated is never the one doing the rotating.
The IAM setup lives in the platform repository rather than here.

**`tofu-encryption` is different in kind**, not just degree. Rotating an API
token works because the old and new can coexist — they're two valid keys to
the same door. Rotating this passphrase means state already encrypted with
the old one has to get re-encrypted with the new one, or it becomes
permanently unreadable, and this is the one state file that defines every
other credential in the system. OpenTofu's own encryption feature supports
multiple decryption methods, so a safe two-phase rotation — accept either
during a transition, then drop the old one — is reachable the same way. It is
a migration rather than a re-mint, and that is the whole of the difference.

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

Nothing decides "it is time": **the daily pass re-plans the credentials root at
the last applied commit**, and the plan is empty until the date crosses a
generation boundary. On that day it mints the next generation, retires the one
two back, repoints the items and rewrites CI's secrets — and applying it *is*
the rotation. It runs whenever the branch-protection gate passed, even if a
commit failed earlier in the pass, because a failed apply last Tuesday is not a
reason for a 45-day window to close. It never runs at a commit the loop has not
applied. A rotation failure is a failure: recorded in the ledger, non-zero exit,
alert.

⚠️ **Daily, not on the frequent pass, and the difference is cost.** A 45-day
boundary does not move between one fifteen-minute tick and the next, so
re-planning `credentials/` on every pass spends a full plan run — and, before
credentials arrived as files, three secret reads — to re-derive a date that has
not changed. The frequent pass reports `{"skipped":"rotation runs on the daily
pass"}` rather than staying silent about it, so a reader of one heartbeat can
tell "not due yet" from "not checked here".

The credential that applies everything is the one that most needs to rotate,
so it is minted like the rest, and only the token that mints it is made by
hand. That widens nothing: a credential that can mint any credential could
always have minted this one.

What cannot be rotated is what no API can mint. Those the applier **watches**:
anything inside the warn window, or with no date recorded at all, is in every
heartbeat and every daily alert until it is renewed. The window is
configurable (`EXPIRY_WARN_DAYS`, 30 days by default) — how much lead time a
renewal actually needs varies by provider, so the number is a knob, not a
constant. The nag itself stays daily regardless of the window's width —
a hand-made credential lapsing takes the applier down with it, and a warning
sent once is a warning sent while somebody was asleep.

⚠️ **The sweep runs on the daily pass, and the reason is a rate limit rather
than tidiness.** An expiry date does not move between fifteen-minute ticks, so
sweeping on every pass asks the secret store the same question 288 times a day
— and on 2026-09-07 that was most of what exhausted the service account's
hourly allowance. Backoff does not fix a budget that is structurally too small;
asking less often does. Daily is also the cadence the answer is acted on: the
alert it feeds goes out once a day.

`never` skips the watch entirely, and it should be rare. The knob changes how
much notice you get; it doesn't change the recommendation. Most hand-made
credentials aren't the kind that should genuinely outlive every renewal
cycle — they're the kind that should carry a real date, sized to whatever
lead time `EXPIRY_WARN_DAYS` is set to, and actually get rotated on it.

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

Revoking the **previous** generation is the ordinary response to a leak,
because nothing should still be using it — everything already picked up the
current one during its overlap window, so destroying the previous one costs
nothing and mints nothing to replace it, on purpose.

The current generation is different: revoking it with nothing else in the
commit is an outage waiting to happen, consumers still hold it, so that's
refused outright. What a leaked *current* generation actually needs is the
same thing rotation always does when a boundary passes — mint a fresh
generation to replace it — just forced early instead of waiting for the
45-day clock. That's what moving the epoch in the same commit does: the
apply that destroys the compromised tokens is the same apply that mints
their non-compromised replacement, so the service comes back up on a key
nobody's seen.

## The credentials root is the one CI never plans

The applier plans and applies every root the same way — except this one,
which CI never plans at all, because CI isn't allowed to read its state. That
state *is* the tokens. So what a human reviews here is the code diff itself,
not a plan, and for a credential that costs nothing: a token resource's
permissions sit right there in the HCL, the same thing a plan would have
shown anyway. It's exempt from the digest gate for the same reason. And it's
the one root the applier re-plans on every daily pass, so a rotation is never a
change nobody signed off on.

More generally: **what a root's state contains decides who may read it.** The
other roots are forbidden, by a CI-enforced rule, from declaring any credential
resource, so their state is safe for the read-only plan tier. Every token in
the system is declared in the one root whose state is encrypted.
