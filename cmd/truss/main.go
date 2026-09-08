// Command truss composes the internal/ packages into the applier CLI
// described in §4.9. It is deliberately thin: every real
// decision lives in internal/config, internal/gates, internal/plan,
// internal/forge, internal/ledger, internal/secrets and internal/notify;
// this file and its siblings in package main only wire them together and
// translate the result into an exit code.
//
// os.Exit is called from exactly one place -- here -- so run's own logic is
// testable without ending the test process (§4.9, "Structure the binary so
// this is testable").
package main

import (
	"os"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
