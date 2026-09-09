---
name: gate
description: Add or change a refusal in internal/gates. Use when a new condition should stop an apply, when a protection field is added or read back, or when a docs table claims a check the code does not make.
---

# Adding a gate

A defect in truss is an apply that should have been refused. A gate that fails
open reports success, so the bar for a new one is a test that has been watched
go red.

## The shape

`internal/gates` performs no I/O. A gate is a pure function over state
somebody else fetched, returning refusal strings. Fetching belongs in
`internal/forge`, `internal/ledger` or `internal/secrets`; deciding belongs
here. That split is what makes "an approval on an earlier push does not count"
a function call rather than a whole-pass run with a ledger read afterwards.

## The checklist

1. **The state type carries pointers for tri-state fields.** Absent, `false`
   and `true` are three facts. Say in a comment which answer absent gives and
   why — it is usually "refuse", and `BypassAllowances` is the exception that
   proves it must be stated.
2. **The refusal names the cause and, where one exists, the ledger key or the
   value that disagreed.** "Refuses one commit for a reason that names the
   wrong cause" is the failure mode; a refusal is read by somebody who cannot
   see the state.
3. **Every field is checked.** `TestEveryProtectionFieldIsChecked` exists
   because a field added to the type and never read makes the per-pass re-read
   a lie. Add to it, and to `TestEveryGateRefusesTheZeroValue`.
4. **Both halves of the test.** The good input passes; the good input with one
   field turned off is refused, one field at a time. Only the second half makes
   the first evidence.
5. **Decode the field where it is fetched.** A gate reading a field
   `wireProtection` never decodes always sees the zero value.
6. **If a script sets it, the script and the gate are tested together.**
   `scripts/protection payload` prints its own JSON and
   `TestProtectionScriptSatisfiesTheGate` feeds it through the real gate, so
   neither side can move without the other noticing. A field the script sets
   and the gate never reads back is the same drift in the other direction.
7. **Gate on the field, never on rendered text.** A regex over YAML once
   reported "does not set LEDGER_BUCKET" about a manifest that set it on the
   next line. If the only route to a check is a parser or a regex over source,
   stop and write down why in `docs/work-items.md` instead — a gate that is
   subtly wrong fails open, which is worse than the gap.
8. **Update the claim.** `docs/design.md` lists what the applier requires;
   `docs/threat-model.md` says what each gate stops. A row there with no
   function behind it is the defect the `claim-auditor` agent hunts.
9. **Run `/watch-it-fail`, then `scripts/check`.**
