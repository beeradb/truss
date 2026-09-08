package repo

import (
	"reflect"
	"testing"
)

func TestTouchedRootsOrderIsCredentialsPlatformProjectsSorted(t *testing.T) {
	changed := []string{
		"projects/zebra/main.tf",
		"platform/main.tf",
		"credentials/main.tf",
		"projects/alpha/main.tf",
	}
	got := TouchedRoots(changed, nil)
	want := []string{"credentials", "platform", "projects/alpha", "projects/zebra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedRoots() = %v, want %v", got, want)
	}
}

func TestASharedInputPlansEveryRootInTheTree(t *testing.T) {
	changed := []string{"modules/vpc/main.tf"}
	tree := []string{"projects/beta", "platform", "projects/alpha"}
	got := TouchedRoots(changed, tree)
	want := []string{"platform", "projects/alpha", "projects/beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedRoots() = %v, want %v", got, want)
	}
}

func TestASharedInputAloneDoesNotIncludeCredentials(t *testing.T) {
	changed := []string{".opentofu-version"}
	tree := []string{"platform", "projects/alpha"}
	got := TouchedRoots(changed, tree)
	for _, r := range got {
		if r == "credentials" {
			t.Fatalf("TouchedRoots() = %v, credentials must not appear: only credentials/ changing includes it", got)
		}
	}
	want := []string{"platform", "projects/alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedRoots() = %v, want %v", got, want)
	}
}

func TestASharedInputWithCredentialsIncludesItFirst(t *testing.T) {
	changed := []string{"providers.allow", "credentials/main.tf"}
	tree := []string{"platform", "projects/alpha"}
	got := TouchedRoots(changed, tree)
	want := []string{"credentials", "platform", "projects/alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedRoots() = %v, want %v", got, want)
	}
}

func TestRootDiscoveryDoesNotDoubleTheProjectsPrefix(t *testing.T) {
	changed := []string{"projects/alpha/main.tf"}
	got := TouchedRoots(changed, nil)
	want := []string{"projects/alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedRoots() = %v, want %v (no projects/projects/ doubling)", got, want)
	}
}

func TestADeepFileYieldsItsProjectRootOnce(t *testing.T) {
	changed := []string{
		"projects/alpha/modules/db/main.tf",
		"projects/alpha/nested/deep/file.tf",
		"projects/alpha/main.tf",
	}
	got := TouchedRoots(changed, nil)
	want := []string{"projects/alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TouchedRoots() = %v, want %v (one root, not one per file)", got, want)
	}
}

func TestACommitTouchingNoRootYieldsNone(t *testing.T) {
	changed := []string{"README.md", "docs/runbook.md"}
	got := TouchedRoots(changed, nil)
	if len(got) != 0 {
		t.Fatalf("TouchedRoots() = %v, want none", got)
	}
}

func TestPathsThatMerelyContainARootNameAreIgnored(t *testing.T) {
	changed := []string{
		"docs/platform/notes.md",  // does not start with platform/
		"docs/credentials/x.md",   // does not start with credentials/
		"not-modules/x.tf",        // does not start with modules/
		"scripts/providers.allow", // providers.allow must be the whole path
		"archive/.opentofu-version",
		"projects-legacy/alpha/main.tf", // not projects/<name>/
	}
	got := TouchedRoots(changed, nil)
	if len(got) != 0 {
		t.Fatalf("TouchedRoots() = %v, want none: matching is anchored at the start of the path", got)
	}
}
