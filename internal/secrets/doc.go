// Package secrets is the applier's read path for credentials: files rendered
// under a mount (Dir, unchanged from secret_field/secret_field_if_present),
// and the daily expiry sweep that used to shell out to `op` and now speaks
// Vault KV v2 directly over net/http (decision 4, docs/port-plan.md §4.7).
//
// Two things this package is deliberately NOT. It is not a Vault client:
// Store can only list item names and read one metadata field, never a
// secret value -- the applier already has secrets, from Dir, and the sweep
// exists to say when they expire, not to fetch them again. And it is not
// the thing that decides what happens on failure: Sweep.Run returns an
// error like any other function. Nothing in this package calls os.Exit --
// see the package's own tests (TestTheSweepFailureIsReturnedNotFatal) and
// §2.16 of the plan, which the sweep exists to satisfy: "the expiry sweep
// never reports a clean bill it did not earn."
package secrets
