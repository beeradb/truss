---
name: watch-it-fail
description: Prove a check can fail before trusting it. Use whenever you add or edit any guard — a gate, a test, an assertion, a scanner pattern, a CI step. A check nobody has watched go red is a claim, not a check.
---

# Watch it fail

A guard that cannot fail is worse than no guard, because it reports success.
Every one of this repository's worst defects was that: two scanner patterns
inert in green CI, a commit-trailer guard vacuous for its whole life while
seven trailers sat in history, a test that passed because one laptop had a
kubectl context named `vault`.

## The procedure

1. **Name what it refuses**, in one sentence, before touching it.
2. **Break exactly that**, in the working tree — invert the field, delete the
   key, revert the fix the guard was written for.
3. **Run the guard and read the message.** Red is not enough. It must name the
   cause you broke. A refusal naming the wrong cause is a defect of its own.
4. **Restore, and run it green.**
5. **Leave the negative case behind as a test**, so step 2 never has to be done
   by hand again. `TestTheGateRefusesEachFieldTheScriptSets` is the pattern:
   take the good input, turn off one field at a time, require each refusal.

## What has actually gone wrong here

- **`grep -q` under `set -o pipefail`.** `-q` exits at the first match, the
  writer dies of SIGPIPE, pipefail hands the `if` that failure — so the guard
  reads false precisely when there is something to find. Use `grep -c`.
- **A fixture too small to reproduce that.** With one small commit the writer
  finishes before the reader exits, nothing gets a signal, and the broken guard
  passes. Put the bulk *behind* the match.
- **`(?i)` in an ERE.** GNU grep warns and matches nothing; some greps accept
  it, so it looks fine locally. Use the `-i` flag.
- **Cached test results.** `-count=1`, always.
- **A skip that reads as a pass.** `t.Skip` when a tool or a live backend is
  absent means the check did not run. Make it fatal where it must run
  (`TRUSS_REQUIRE_JQ=1`), and say which ones were skipped.
- **A test that depends on the machine.** If it passes because of something on
  this box, it is testing the box.

## Absent is its own case

A missing key and an explicit `false` are different facts. `// true` in jq
fires on `false` as readily as on `null`, which turned a compliant repository
non-compliant. Test the absent case separately, every time — and say which
answer absent should give, since it is not always "refuse":
`BypassPullRequestAllowances` is nil when nobody may bypass.
