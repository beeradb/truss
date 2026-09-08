package main

import (
	"fmt"
	"io"

	"github.com/beeradb/truss/internal/plan"
)

// cmdPlanDigest is the digest as a command: read a `tofu show -json` plan
// from stdin, print its digest to stdout with no trailing newline (§4.9,
// TestPlanDigestReadsStdinAndWritesNoTrailingNewline), exit 0. Any error in
// reading stdin or computing the digest is a refusal: exit 1, message on
// stderr.
func cmdPlanDigest(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss plan-digest < plan.json")
		return 2
	}
	planJSON, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "plan-digest: reading stdin: %v\n", err)
		return 1
	}
	digest, err := plan.Digest(planJSON)
	if err != nil {
		fmt.Fprintf(stderr, "plan-digest: %v\n", err)
		return 1
	}
	fmt.Fprint(stdout, digest)
	return 0
}
