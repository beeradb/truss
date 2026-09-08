// Package repo derives which infrastructure roots a commit touches, and
// (elsewhere, not in this file) drives the git binary that backs it.
package repo

import (
	"regexp"
	"sort"
)

// sharedInput matches a path that is an input to EVERY root: the shared
// modules tree, the provider allowlist, and the pinned opentofu version.
// Anchored at the start of the path, same as every pattern below -- a commit
// touching "docs/modules/README.md" must not be read as touching the shared
// modules tree.
var sharedInput = regexp.MustCompile(`^(modules/|providers\.allow$|\.opentofu-version$)`)

var credentialsPath = regexp.MustCompile(`^credentials/`)
var platformPath = regexp.MustCompile(`^platform/`)
var projectPath = regexp.MustCompile(`^projects/([^/]+)/`)

// TouchedRoots derives which roots a commit touches from the files it
// changed and the roots that exist in the commit's own tree (already
// filtered to "platform" and "projects/<name>" entries by whatever read the
// tree -- this function does no filesystem or git I/O of its own).
//
// The rule: if any changed path matches a shared input, the result is
// "credentials" (only if
// "credentials/" also changed) followed by every root in treeRoots, sorted,
// and nothing else. Otherwise: "credentials" if "credentials/" changed,
// "platform" if "platform/" changed, then each distinct "projects/<name>"
// named under "projects/", sorted. Matching is anchored at the start of the
// path.
func TouchedRoots(changedFiles, treeRoots []string) []string {
	sharedChanged := false
	credentialsChanged := false
	for _, f := range changedFiles {
		if sharedInput.MatchString(f) {
			sharedChanged = true
		}
		if credentialsPath.MatchString(f) {
			credentialsChanged = true
		}
	}

	var roots []string
	if credentialsChanged {
		roots = append(roots, "credentials")
	}

	if sharedChanged {
		return append(roots, sortedUnique(treeRoots)...)
	}

	platformChanged := false
	projects := make(map[string]bool)
	for _, f := range changedFiles {
		if platformPath.MatchString(f) {
			platformChanged = true
		}
		if m := projectPath.FindStringSubmatch(f); m != nil {
			projects[m[1]] = true
		}
	}
	if platformChanged {
		roots = append(roots, "platform")
	}
	names := make([]string, 0, len(projects))
	for p := range projects {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		roots = append(roots, "projects/"+p)
	}
	return roots
}

// sortedUnique sorts s and drops duplicates, without mutating the caller's
// slice.
func sortedUnique(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	sorted := append([]string(nil), s...)
	sort.Strings(sorted)
	out := make([]string, 0, len(sorted))
	var last string
	for i, v := range sorted {
		if i > 0 && v == last {
			continue
		}
		out = append(out, v)
		last = v
	}
	return out
}
