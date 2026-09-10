package main

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"github.com/beeradb/truss/internal/inventory"
)

// checkInventoryAtCommit enforces the inventory at one commit: the head
// tree must be internally consistent (inventory.Check), and -- when the
// commit has a parent whose inventory can also be read cleanly -- a
// stateful environment must not have moved to a different cluster or host
// between the two (inventory.CheckMoves). It returns the reason to refuse
// the commit, or empty. runCommitLoop calls this before any root is
// derived or any unit rendered; see its own comment for why the order
// matters.
//
// ⚠️ TWO SKIPS, BOTH DELIBERATE, BOTH THE SAME SHAPE publishDeliveryRef
// ALREADY USES: A DEPLOYMENT THAT HAS NOT ADOPTED A FEATURE IS NOT REFUSED
// FOR NOT USING IT.
//
//  1. A tree with no inventory/ directory is not refused. inventory.Load
//     reports an absent inventory/ as a problem -- right for `truss
//     inventory validate`, pointed deliberately at a platform checkout,
//     and wrong here, where it would refuse every commit from a deployment
//     that has never adopted the inventory at all. Checked with fs.Stat
//     before Load is even called, so Load's own "absent is a problem"
//     behaviour is never reached on this path. internal/parity's 43
//     recorded scenarios carry no inventory/ directory at all; without
//     this skip every one of them refuses.
//
//  2. A commit with no parent, or a parent whose inventory cannot be read
//     cleanly (most often because the parent predates the inventory's own
//     adoption and has no inventory/ directory of its own), skips
//     CheckMoves only -- there is nothing comparable to check against.
//     Check on the head still runs regardless: the head's own consistency
//     does not depend on having a comparable parent, and refusing to even
//     look at it because the parent has nothing to compare would be a
//     second, unrelated hole.
func checkInventoryAtCommit(ctx context.Context, d applyDeps, headSHA string) string {
	headFS, err := d.Git.TreeFS(ctx, headSHA)
	if err != nil {
		return fmt.Sprintf("could not read the inventory tree at %s: %v", headSHA, err)
	}
	if _, err := fs.Stat(headFS, "inventory"); err != nil {
		// This deployment has not adopted the inventory. See ⚠️ 1 above.
		return ""
	}

	head, loadProblems := inventory.Load(headFS)
	problems := append(append([]string(nil), loadProblems...), inventory.Check(head)...)
	if len(problems) > 0 {
		return strings.Join(problems, "; ")
	}

	parentSHA, ok := d.Git.Parent(ctx, headSHA)
	if !ok {
		// A root commit: nothing to compare against. See ⚠️ 2 above.
		return ""
	}
	parentFS, err := d.Git.TreeFS(ctx, parentSHA)
	if err != nil {
		// The parent could not be read at all; skip the comparison rather
		// than refuse the head commit for a problem that is, at worst, the
		// parent's. See ⚠️ 2 above.
		return ""
	}
	parent, parentProblems := inventory.Load(parentFS)
	if len(parentProblems) > 0 {
		// The parent's own inventory did not load cleanly -- see ⚠️ 2 above
		// for why that is not this commit's problem to be refused for.
		return ""
	}

	if moveProblems := inventory.CheckMoves(parent, head); len(moveProblems) > 0 {
		return strings.Join(moveProblems, "; ")
	}
	return ""
}
