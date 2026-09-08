package gates

import (
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// --- fixtures -----------------------------------------------------------
//
// Fake shas throughout are deliberately NOT hex: "sha1", "headsha1", never
// anything that could pass for a real 32+ character hex id. A fixture that
// looks like a commit is a fixture somebody eventually goes looking for in
// the real repository.

func boolPtr(b bool) *bool    { return &b }
func intPtr(i int) *int       { return &i }
func strPtr(s string) *string { return &s }

// compliantProtection is a Protection that clears CheckProtection's bar
// exactly, so every other test in this file can start from "passing" and
// break one thing.
func compliantProtection() Protection {
	return Protection{
		RequiredApprovals:     intPtr(1),
		RequireCodeOwners:     boolPtr(true),
		DismissStaleReviews:   boolPtr(true),
		EnforceAdmins:         boolPtr(true),
		AllowForcePushes:      boolPtr(false),
		RequireUpToDateBranch: boolPtr(true),
		StatusChecks:          []string{"plan"},
	}
}

func hasProblemContaining(problems []string, substr string) bool {
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// --- CheckProtection ------------------------------------------------------

func TestCheckProtectionRefusesEachMissingSetting(t *testing.T) {
	base := compliantProtection()
	if problems := CheckProtection(base, "plan"); len(problems) != 0 {
		t.Fatalf("the compliant baseline was refused: %v", problems)
	}

	cases := []struct {
		name    string
		mutate  func(*Protection)
		wantSub string
	}{
		{"approvals below one", func(p *Protection) { p.RequiredApprovals = intPtr(0) }, "required_approving_review_count is below 1"},
		{"code owners off", func(p *Protection) { p.RequireCodeOwners = boolPtr(false) }, "require_code_owner_reviews is off"},
		{"dismiss stale off", func(p *Protection) { p.DismissStaleReviews = boolPtr(false) }, "dismiss_stale_reviews is off"},
		{"enforce admins off", func(p *Protection) { p.EnforceAdmins = boolPtr(false) }, "enforce_admins is off"},
		{"force pushes on", func(p *Protection) { p.AllowForcePushes = boolPtr(true) }, "allow_force_pushes is on, or unreadable"},
		{"branch not required up to date", func(p *Protection) { p.RequireUpToDateBranch = boolPtr(false) }, "required_status_checks.strict is off"},
		{"required check missing", func(p *Protection) { p.StatusChecks = []string{"other"} }, `required status check "plan" is not required`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := compliantProtection()
			c.mutate(&p)
			problems := CheckProtection(p, "plan")
			if !hasProblemContaining(problems, c.wantSub) {
				t.Fatalf("problems %v do not mention %q", problems, c.wantSub)
			}
		})
	}
}

func TestAMissingKeyIsNotReadAsCompliant(t *testing.T) {
	// Every optional field of Protection, left nil (a "missing key" in the
	// forge's JSON), must refuse. AllowForcePushes is the interesting case:
	// its COMPLIANT value is false, so a naive truthiness test would let a
	// missing key through exactly where the field's own doc comment warns
	// that this goes wrong.
	cases := []struct {
		name   string
		mutate func(*Protection)
	}{
		{"required approvals nil", func(p *Protection) { p.RequiredApprovals = nil }},
		{"require code owners nil", func(p *Protection) { p.RequireCodeOwners = nil }},
		{"dismiss stale reviews nil", func(p *Protection) { p.DismissStaleReviews = nil }},
		{"enforce admins nil", func(p *Protection) { p.EnforceAdmins = nil }},
		{"allow force pushes nil", func(p *Protection) { p.AllowForcePushes = nil }},
		{"require up to date branch nil", func(p *Protection) { p.RequireUpToDateBranch = nil }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := compliantProtection()
			c.mutate(&p)
			if problems := CheckProtection(p, "plan"); len(problems) == 0 {
				t.Fatalf("a missing key was read as compliant")
			}
		})
	}
}

func TestAnExplicitFalseAllowForcePushesIsCompliant(t *testing.T) {
	p := compliantProtection()
	p.AllowForcePushes = boolPtr(false)
	if problems := CheckProtection(p, "plan"); len(problems) != 0 {
		t.Fatalf("an explicit false allow_force_pushes was refused: %v", problems)
	}
}

func TestBothStatusCheckShapesSatisfyTheGate(t *testing.T) {
	// The forge merges `required_status_checks.contexts` (plain strings) and
	// `.checks[].context` (objects) into one StatusChecks list before gates
	// ever sees it (§4.6). This asserts CheckProtection is indifferent to
	// which shape a name arrived from: it is a membership test, nothing more.
	asIfFromContexts := compliantProtection()
	asIfFromContexts.StatusChecks = []string{"plan"}

	asIfFromChecksObjects := compliantProtection()
	asIfFromChecksObjects.StatusChecks = []string{"lint", "plan"}

	for _, p := range []Protection{asIfFromContexts, asIfFromChecksObjects} {
		if problems := CheckProtection(p, "plan"); len(problems) != 0 {
			t.Fatalf("a required check present in StatusChecks was refused: %v", problems)
		}
	}
}

func TestEveryProtectionFieldIsChecked(t *testing.T) {
	base := compliantProtection()
	if problems := CheckProtection(base, "plan"); len(problems) != 0 {
		t.Fatalf("the compliant baseline was refused: %v", problems)
	}

	typ := reflect.TypeOf(base)
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		t.Run(name, func(t *testing.T) {
			mutated := compliantProtection()
			v := reflect.ValueOf(&mutated).Elem()
			field := v.Field(i)
			field.Set(reflect.Zero(field.Type()))

			problems := CheckProtection(mutated, "plan")
			if len(problems) == 0 {
				t.Fatalf("zeroing Protection.%s did not change the result -- this field is not checked", name)
			}
		})
	}
}

// --- CheckApproval ---------------------------------------------------------

func TestApprovalOnAnEarlierPushDoesNotCount(t *testing.T) {
	pr := PullRequest{Number: 1, Merged: true, MergeCommitSHA: "sha1", HeadSHA: "headsha1"}
	reviews := []Review{
		{User: "alice", State: "APPROVED", CommitID: "oldsha1"},
	}

	problems := CheckApproval(pr, reviews, "alice", "sha1")
	if !hasProblemContaining(problems, "no approval by alice at the PR head headsha1") {
		t.Fatalf("an approval on an earlier push was counted: %v", problems)
	}
}

func TestApprovalByAnyoneElseDoesNotCount(t *testing.T) {
	pr := PullRequest{Number: 1, Merged: true, MergeCommitSHA: "sha1", HeadSHA: "headsha1"}
	reviews := []Review{
		{User: "mallory", State: "APPROVED", CommitID: "headsha1"},
	}

	problems := CheckApproval(pr, reviews, "alice", "sha1")
	if !hasProblemContaining(problems, "no approval by alice at the PR head headsha1") {
		t.Fatalf("an approval by someone other than the approver was counted: %v", problems)
	}
}

func TestAnUnmergedPRIsRefused(t *testing.T) {
	pr := PullRequest{Number: 7, Merged: false, MergeCommitSHA: "sha1", HeadSHA: "headsha1"}
	reviews := []Review{
		{User: "alice", State: "APPROVED", CommitID: "headsha1"},
	}

	problems := CheckApproval(pr, reviews, "alice", "sha1")
	if !hasProblemContaining(problems, "PR #7 is not merged") {
		t.Fatalf("an unmerged PR was accepted: %v", problems)
	}
}

func TestAMergeCommitThatIsNotThePRsIsRefused(t *testing.T) {
	pr := PullRequest{Number: 3, Merged: true, MergeCommitSHA: "shaA", HeadSHA: "headsha1"}
	reviews := []Review{
		{User: "alice", State: "APPROVED", CommitID: "headsha1"},
	}

	problems := CheckApproval(pr, reviews, "alice", "shaB")
	if !hasProblemContaining(problems, "merge commit is shaA, not shaB") {
		t.Fatalf("a merge commit that was not the PR's own was accepted: %v", problems)
	}
}

// TestAnEmptyMergeCommitSHAIsRefused was specified in §7.8 on the
// assumption that an absent merge_commit_sha being read as compliant was a
// bug. The owner has confirmed it is, and gates.go has been fixed: this test
// was watched failing against the pre-fix line before the fix landed (see
// the report), and now passes.
func TestAnEmptyMergeCommitSHAIsRefused(t *testing.T) {
	pr := PullRequest{Number: 5, Merged: true, MergeCommitSHA: "", HeadSHA: "headsha1"}
	reviews := []Review{
		{User: "alice", State: "APPROVED", CommitID: "headsha1"},
	}

	problems := CheckApproval(pr, reviews, "alice", "sha1")
	if !hasProblemContaining(problems, "merge commit") {
		t.Fatalf("an empty merge_commit_sha was read as compliant: %v", problems)
	}
}

// TestAStaleApprovalAlongsideAFreshOneIsIgnored resolves §7.9. Only reviews
// AT THE HEAD SHA are counted; a stale review outside that filter produces
// no message at all, so a fresh APPROVED review still merges cleanly. The
// owner's ruling: leave it that way.
// dismiss_stale_reviews already dismisses a review on push, and the
// applier separately requires an approval at the head, so a stale entry
// sitting in the list is ordinary API noise, not evidence -- refusing on
// it would wedge any PR that got a second push, which is most of them.
func TestAStaleApprovalAlongsideAFreshOneIsIgnored(t *testing.T) {
	pr := PullRequest{Number: 9, Merged: true, MergeCommitSHA: "sha1", HeadSHA: "headsha1"}
	reviews := []Review{
		{User: "alice", State: "APPROVED", CommitID: "oldsha1"},
		{User: "alice", State: "APPROVED", CommitID: "headsha1"},
	}

	problems := CheckApproval(pr, reviews, "alice", "sha1")
	if len(problems) != 0 {
		t.Fatalf("a stale approval alongside a fresh one was refused: %v", problems)
	}
}

// The converse of the above: a stale approval with NOTHING fresh at the
// head sha must still refuse. Ignoring the stale entry must not be
// mistaken for ignoring the requirement it was noise beside.
func TestAStaleApprovalAloneIsRefused(t *testing.T) {
	pr := PullRequest{Number: 9, Merged: true, MergeCommitSHA: "sha1", HeadSHA: "headsha1"}
	reviews := []Review{
		{User: "alice", State: "APPROVED", CommitID: "oldsha1"},
	}

	problems := CheckApproval(pr, reviews, "alice", "sha1")
	if !hasProblemContaining(problems, "no approval by alice at the PR head headsha1") {
		t.Fatalf("a PR with only a stale approval was accepted: %v", problems)
	}
}

// --- CheckMergeCommit -------------------------------------------------------

func TestCheckMergeCommitRequiresTheForgesOwnMerge(t *testing.T) {
	webflow := "web-flow"
	someoneElse := "octocat"

	compliant := Commit{SHA: "sha1", Verified: boolPtr(true), CommitterLogin: strPtr(webflow)}
	if problems := CheckMergeCommit(compliant); len(problems) != 0 {
		t.Fatalf("a verified web-flow merge was refused: %v", problems)
	}

	notVerified := Commit{SHA: "sha1", Verified: boolPtr(false), CommitterLogin: strPtr(webflow)}
	if problems := CheckMergeCommit(notVerified); len(problems) == 0 {
		t.Fatalf("an unverified merge commit was accepted")
	}

	wrongCommitter := Commit{SHA: "sha1", Verified: boolPtr(true), CommitterLogin: strPtr(someoneElse)}
	if problems := CheckMergeCommit(wrongCommitter); len(problems) == 0 {
		t.Fatalf("a merge commit not committed by web-flow was accepted")
	}

	absentVerified := Commit{SHA: "sha1", Verified: nil, CommitterLogin: strPtr(webflow)}
	if problems := CheckMergeCommit(absentVerified); len(problems) == 0 {
		t.Fatalf("an absent verification flag was read as compliant")
	}

	absentCommitter := Commit{SHA: "sha1", Verified: boolPtr(true), CommitterLogin: nil}
	if problems := CheckMergeCommit(absentCommitter); len(problems) == 0 {
		t.Fatalf("an absent committer login was read as compliant")
	}
}

// --- CheckPlanDigest ---------------------------------------------------------

func TestCheckPlanDigestRefusesWhenNoneWasRecorded(t *testing.T) {
	problems := CheckPlanDigest("platform", "headsha1", "digests/headsha1/platform.digest", "minedigest", "", false)
	if len(problems) == 0 {
		t.Fatalf("a missing recorded digest was accepted")
	}
}

func TestCheckPlanDigestRefusesAMismatchAndNamesBothValues(t *testing.T) {
	problems := CheckPlanDigest("platform", "headsha1", "digests/headsha1/platform.digest", "minedigest", "theirdigest", true)
	if !hasProblemContaining(problems, "minedigest") || !hasProblemContaining(problems, "theirdigest") {
		t.Fatalf("a mismatch did not name both values: %v", problems)
	}
}

func TestCheckPlanDigestExemptsOnlyTheCredentialsRoot(t *testing.T) {
	if problems := CheckPlanDigest("credentials", "headsha1", "digests/headsha1/platform.digest", "minedigest", "", false); len(problems) != 0 {
		t.Fatalf("the credentials root was not exempted from the digest gate: %v", problems)
	}
	if problems := CheckPlanDigest("platform", "headsha1", "digests/headsha1/platform.digest", "minedigest", "", false); len(problems) == 0 {
		t.Fatalf("a non-credentials root with no recorded digest was exempted")
	}
}

// --- cross-cutting -----------------------------------------------------------

func TestEveryGateRefusesTheZeroValue(t *testing.T) {
	if problems := CheckProtection(Protection{}, "plan"); len(problems) == 0 {
		t.Errorf("CheckProtection accepted the zero value")
	}
	if problems := CheckApproval(PullRequest{}, nil, "", ""); len(problems) == 0 {
		t.Errorf("CheckApproval accepted the zero value")
	}
	if problems := CheckMergeCommit(Commit{}); len(problems) == 0 {
		t.Errorf("CheckMergeCommit accepted the zero value")
	}
	if problems := CheckPlanDigest("", "", "digests/headsha1/platform.digest", "", "", false); len(problems) == 0 {
		t.Errorf("CheckPlanDigest accepted the zero value")
	}
}

func TestGatesImportsNothingThatDoesIO(t *testing.T) {
	// Package gates's own doc comment: "nothing here performs I/O: each gate
	// is a pure function over state somebody else fetched." An import is the
	// cheapest way that promise breaks silently, so pin it to the exact set
	// this package needs today rather than to a denylist that a future I/O
	// package might not be on.
	allowed := map[string]bool{"fmt": true, "time": true}

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing internal/gates: %v", err)
	}

	for _, pkg := range pkgs {
		for fname, file := range pkg.Files {
			for _, imp := range file.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("%s: unparseable import literal %s", fname, imp.Path.Value)
				}
				if !allowed[path] {
					t.Errorf("%s imports %q: gates.go performs no I/O and must import nothing that does", fname, path)
				}
			}
		}
	}
}

// TestCheckPlanDigestTellsAnEmptyDigestApartFromAMismatch: an approved
// digest that is recorded but EMPTY means nobody reviewed it, not that the
// plan changed. It used to fall through to the mismatch branch and print
// "(approved , ours <digest>): the world moved between review and apply" --
// a sentence describing a race that did not happen, with a blank where a
// digest should be.
//
// Both outcomes refuse, so nothing was unsafe; what was wrong was telling
// the operator the wrong story. The doc comment had called this case
// "impossible in practice", which is the kind of claim that stops anyone
// handling it. Found by the 2026-09-08 code audit.
func TestCheckPlanDigestTellsAnEmptyDigestApartFromAMismatch(t *testing.T) {
	const key = "digests/headsha1/platform.digest"

	empty := CheckPlanDigest("platform", "headsha1", key, "minedigest", "", true)
	if len(empty) == 0 {
		t.Fatal("an empty recorded digest was accepted")
	}
	if !hasProblemContaining(empty, "nobody reviewed") {
		t.Errorf("an empty recorded digest is not reported as unreviewed: %v", empty)
	}
	if hasProblemContaining(empty, "the world moved") {
		t.Errorf("an empty recorded digest is reported as a mismatch: %v", empty)
	}

	mismatch := CheckPlanDigest("platform", "headsha1", key, "minedigest", "theirdigest", true)
	if !hasProblemContaining(mismatch, "the world moved") {
		t.Errorf("a real mismatch is no longer reported as one: %v", mismatch)
	}
}

// TestAnUnreviewedRefusalNamesTheLedgerKey: the key is what an operator goes
// and looks at when nothing was recorded, so this refusal names it.
// Deliberately only this one -- a mismatch already prints both digests, and
// adding the key there would be noise in an alert that already says
// everything it needs to.
func TestAnUnreviewedRefusalNamesTheLedgerKey(t *testing.T) {
	const key = "digests/headsha1/platform.digest"
	for _, tc := range []struct {
		name     string
		approved string
		found    bool
	}{
		{"nothing recorded", "", false},
		{"recorded but empty", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := CheckPlanDigest("platform", "headsha1", key, "minedigest", tc.approved, tc.found)
			if len(problems) == 0 {
				t.Fatal("accepted, so there is no refusal to inspect")
			}
			if !hasProblemContaining(problems, key) {
				t.Errorf("the refusal does not name the ledger key: %v", problems)
			}
		})
	}
}

// TestApprovalWithdrawnAtTheSameHeadDoesNotCount: approve, spot something,
// request changes on the same push -- the gate must not still read
// "approved". Scanning for any APPROVED review counts an approval that was
// afterwards withdrawn.
//
// ⚠️ SO CHANGES_REQUESTED AND DISMISSED ARE READ, NOT JUST APPROVED: a gate
// that counts only APPROVED cannot see a withdrawal. GitHub's own protection
// would block such a merge, but this gate exists precisely because it does
// not take the merge's legitimacy on trust. Raised by the 2026-09-08
// security review.
func TestApprovalWithdrawnAtTheSameHeadDoesNotCount(t *testing.T) {
	pr := PullRequest{Number: 42, Merged: true, MergeCommitSHA: "mergesha", HeadSHA: "headsha1"}

	withdrawn := []Review{
		{State: "APPROVED", User: "alice", CommitID: "headsha1"},
		{State: "CHANGES_REQUESTED", User: "alice", CommitID: "headsha1"},
	}
	if problems := CheckApproval(pr, withdrawn, "alice", "mergesha"); len(problems) == 0 {
		t.Error("an approval withdrawn by CHANGES_REQUESTED at the same head still counted")
	}

	// Re-approving after requesting changes must count again -- the last
	// word decides, in both directions.
	reapproved := []Review{
		{State: "APPROVED", User: "alice", CommitID: "headsha1"},
		{State: "CHANGES_REQUESTED", User: "alice", CommitID: "headsha1"},
		{State: "APPROVED", User: "alice", CommitID: "headsha1"},
	}
	if problems := CheckApproval(pr, reapproved, "alice", "mergesha"); len(problems) != 0 {
		t.Errorf("a re-approval after requested changes was not accepted: %v", problems)
	}

	// A COMMENTED review says nothing about approval and must not clear one.
	commented := []Review{
		{State: "APPROVED", User: "alice", CommitID: "headsha1"},
		{State: "COMMENTED", User: "alice", CommitID: "headsha1"},
	}
	if problems := CheckApproval(pr, commented, "alice", "mergesha"); len(problems) != 0 {
		t.Errorf("a comment after an approval cleared it: %v", problems)
	}

	// Somebody else requesting changes is not the approver withdrawing.
	otherPerson := []Review{
		{State: "APPROVED", User: "alice", CommitID: "headsha1"},
		{State: "CHANGES_REQUESTED", User: "bob", CommitID: "headsha1"},
	}
	if problems := CheckApproval(pr, otherPerson, "alice", "mergesha"); len(problems) != 0 {
		t.Errorf("another user's CHANGES_REQUESTED cleared alice's approval: %v", problems)
	}
}

// TestAnEmptyHeadSHAIsRefusedRatherThanMatched: `r.CommitID != pr.HeadSHA`
// is FALSE when both are empty, so a PR with no head sha plus a review
// carrying no commit id read as APPROVED. Absent is not agreement -- the
// rule this file applies to every other absent field, and the one place it
// was not applied. Raised by the 2026-09-08 security review.
func TestAnEmptyHeadSHAIsRefusedRatherThanMatched(t *testing.T) {
	pr := PullRequest{Number: 42, Merged: true, MergeCommitSHA: "mergesha", HeadSHA: ""}
	reviews := []Review{{State: "APPROVED", User: "alice", CommitID: ""}}

	problems := CheckApproval(pr, reviews, "alice", "mergesha")
	if len(problems) == 0 {
		t.Fatal("a PR with no head sha was approved by a review with no commit id")
	}
	if !hasProblemContaining(problems, "no head sha") {
		t.Errorf("the refusal does not name the missing head sha: %v", problems)
	}
}
