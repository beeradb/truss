package inventory

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Load reads an inventory tree and the delivery directories beside it,
// returning the Snapshot and every problem found while reading. A caller
// runs Check over the result; Load reports only what stopped a record being
// read at all.
//
// ⚠️ IT READS THROUGH AN fs.FS AND NEVER THE REAL FILESYSTEM. This package
// still imports no "os", which is what TestInventoryPerformsNoIO actually
// enforces and what makes the promise checkable: Load cannot open a path the
// caller did not hand it, cannot follow one out of the tree, and is driven in
// tests by fstest.MapFS rather than by a temp directory. The caller supplies
// os.DirFS(checkout) in production.
//
// Problems are returned rather than an error because a tree with three bad
// records should report three, not the first -- the same rule config.Load
// keeps.
func Load(fsys fs.FS) (Snapshot, []string) {
	s := Snapshot{
		Hosts:        map[string]Host{},
		Clusters:     map[string]Cluster{},
		Projects:     map[string]Project{},
		Environments: map[string]Environment{},
	}
	var problems []string

	// An absent inventory/ is refused. A platform with no inventory at all
	// is not an empty platform, it is a tree this build cannot make sense
	// of, and treating it as "nothing declared" would let every consistency
	// check below pass over a repository that never wired any of this up.
	if _, err := fs.Stat(fsys, "inventory"); err != nil {
		return s, []string{"inventory/: no inventory directory — create it, or point truss at the platform checkout"}
	}

	for stem, data := range readJSON(fsys, "inventory/hosts", &problems) {
		h, err := DecodeHost(stem, data)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		s.Hosts[stem] = h
	}
	for stem, data := range readJSON(fsys, "inventory/clusters", &problems) {
		c, err := DecodeCluster(stem, data)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		s.Clusters[stem] = c
	}
	for stem, data := range readJSON(fsys, "inventory/projects", &problems) {
		p, err := DecodeProject(stem, data)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		s.Projects[stem] = p
	}

	// Environments are nested one level deeper: inventory/environments/
	// <project>/<name>.json, and the key Check expects is "<project>/<name>".
	for _, project := range subdirs(fsys, "inventory/environments", &problems) {
		dir := path.Join("inventory/environments", project)
		for stem, data := range readJSON(fsys, dir, &problems) {
			key := project + "/" + stem
			e, err := DecodeEnvironment(key, data)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			s.Environments[key] = e
		}
	}

	for _, cluster := range subdirs(fsys, "deliveries", &problems) {
		for _, unit := range subdirs(fsys, path.Join("deliveries", cluster), &problems) {
			s.DeliveryUnits = append(s.DeliveryUnits, cluster+"/"+unit)
		}
	}
	sort.Strings(s.DeliveryUnits)

	sort.Strings(problems)
	return s, problems
}

// readJSON returns every *.json file in dir, keyed by filename stem.
//
// An absent dir is an empty set rather than a problem, and the reason is
// git: it cannot commit an empty directory, so a platform with no hosts yet
// would have to carry a placeholder file to satisfy a stricter rule. What is
// NOT forgiven is a file that is plainly meant to be a record and is not
// readable as one -- see the non-JSON check below.
func readJSON(fsys fs.FS, dir string, problems *[]string) map[string][]byte {
	out := map[string][]byte{}
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			*problems = append(*problems, fmt.Sprintf(
				"%s: a directory where a record belongs — inventory records are single .json files; move or remove it",
				path.Join(dir, name)))
			continue
		}
		// ⚠️ A NON-JSON FILE HERE IS A PROBLEM, NOT SOMETHING TO SKIP. A
		// hosts/beta.yaml or a beta.json.bak would otherwise be silently
		// ignored, and the machine it describes would simply not be
		// managed -- absent read as compliant, in the one place this system
		// is most insistent it must not be.
		if !strings.HasSuffix(name, ".json") {
			*problems = append(*problems, fmt.Sprintf(
				"%s: not a .json record — rename it or remove it; truss reads only .json here",
				path.Join(dir, name)))
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			*problems = append(*problems, fmt.Sprintf(
				"%s: could not be read (%v) — fix the file or remove it", path.Join(dir, name), err))
			continue
		}
		out[strings.TrimSuffix(name, ".json")] = data
	}
	return out
}

// subdirs returns the directory names directly under dir, sorted. An absent
// dir is an empty list, for the same git reason as readJSON.
func subdirs(fsys fs.FS, dir string, problems *[]string) []string {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
