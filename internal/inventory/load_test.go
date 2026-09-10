package inventory

import (
	"strings"
	"testing"
	"testing/fstest"
)

// validTree is the smallest inventory that Load reads and Check accepts, so
// the two halves are exercised together. A loader test that never feeds Check
// proves only that files were opened.
func validTree() fstest.MapFS {
	f := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"inventory/hosts/alpha.json": f(`{
			"schema":"truss.host/v1","name":"alpha","kind":"vm","role":"k8s-node",
			"provisioned_by":"hosts/alpha","config":"ansible/alpha",
			"tailnet_tags":["k8s"],"cluster":"prod","frozen":false,"decommissioned":false}`),
		"inventory/clusters/prod.json": f(`{
			"schema":"truss.cluster/v1","name":"prod","distribution":"k3s",
			"hosts":["alpha"],"baseline":"baselines/prod","capabilities":["ingress"],
			"kubeconfig_item":"prod-kubeconfig","has_ha_vault":true}`),
		"inventory/projects/wren.json": f(`{
			"schema":"truss.project/v1","name":"wren","environments":["prod"]}`),
		"inventory/environments/wren/prod.json": f(`{
			"schema":"truss.environment/v1","project":"wren","name":"prod",
			"shape":"kubernetes","placement":{"cluster":"prod","namespace":"web"},
			"requires":["ingress"],"vault":{"mount":"secret","prefix":"wren/prod"},
			"frozen":false}`),
		"deliveries/prod/web/kustomization.yaml": f("kind: Kustomization\n"),
	}
}

func TestLoadReadsAWholeTreeAndCheckAcceptsIt(t *testing.T) {
	s, problems := Load(validTree())
	if len(problems) != 0 {
		t.Fatalf("Load reported problems on a valid tree: %v", problems)
	}
	if len(s.Hosts) != 1 || len(s.Clusters) != 1 || len(s.Projects) != 1 || len(s.Environments) != 1 {
		t.Fatalf("Load = %+v, want one of each record", s)
	}
	if _, ok := s.Environments["wren/prod"]; !ok {
		t.Errorf("environments keyed %v, want a \"wren/prod\" key", keysOf(s.Environments))
	}
	if len(s.DeliveryUnits) != 1 || s.DeliveryUnits[0] != "prod/web" {
		t.Errorf("DeliveryUnits = %v, want [prod/web]", s.DeliveryUnits)
	}
	if got := Check(s); len(got) != 0 {
		t.Fatalf("Check refused a tree Load read cleanly: %v", got)
	}
}

// TestLoadRefusesAnAbsentInventory covers the difference between "nothing is
// declared" and "this is not a platform checkout". Treating the second as the
// first would let every consistency check pass over a tree that never wired
// any of this up.
func TestLoadRefusesAnAbsentInventory(t *testing.T) {
	_, problems := Load(fstest.MapFS{"README.md": &fstest.MapFile{Data: []byte("hi")}})
	if len(problems) == 0 {
		t.Fatal("Load accepted a tree with no inventory/ directory")
	}
	if !strings.Contains(problems[0], "inventory/") {
		t.Errorf("problem = %q, want it to name the directory", problems[0])
	}
}

// TestLoadReportsANonJSONRecord is the check that stops a machine going
// unmanaged because somebody wrote beta.yaml or left a beta.json.bak.
func TestLoadReportsANonJSONRecord(t *testing.T) {
	tree := validTree()
	tree["inventory/hosts/beta.yaml"] = &fstest.MapFile{Data: []byte("name: beta\n")}
	tree["inventory/hosts/gamma.json.bak"] = &fstest.MapFile{Data: []byte("{}")}

	_, problems := Load(tree)
	if len(problems) != 2 {
		t.Fatalf("problems = %v, want one per non-record file", problems)
	}
	for _, p := range problems {
		if !strings.Contains(p, ".json") {
			t.Errorf("problem = %q, want it to say what truss reads", p)
		}
	}
}

// TestLoadReportsEveryBadRecordNotJustTheFirst: a tree with three broken
// records should report three. Stopping at the first turns fixing an
// inventory into a guessing loop.
func TestLoadReportsEveryBadRecordNotJustTheFirst(t *testing.T) {
	tree := validTree()
	tree["inventory/hosts/bad1.json"] = &fstest.MapFile{Data: []byte(`{`)}
	tree["inventory/hosts/bad2.json"] = &fstest.MapFile{Data: []byte(`{"schema":"truss.host/v1","nope":1}`)}
	tree["inventory/clusters/bad3.json"] = &fstest.MapFile{Data: []byte(`not json at all`)}

	_, problems := Load(tree)
	if len(problems) != 3 {
		t.Fatalf("problems = %v, want three", problems)
	}
}

// TestLoadIgnoresAnAbsentSubdirectory: git cannot commit an empty directory,
// so a platform with no hosts yet would otherwise have to carry a placeholder
// file to satisfy a stricter rule.
func TestLoadIgnoresAnAbsentSubdirectory(t *testing.T) {
	tree := validTree()
	delete(tree, "inventory/hosts/alpha.json")
	s, problems := Load(tree)
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none for an absent hosts directory", problems)
	}
	if len(s.Hosts) != 0 {
		t.Errorf("Hosts = %v, want empty", s.Hosts)
	}
}

func keysOf(m map[string]Environment) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
