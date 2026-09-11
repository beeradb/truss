package ansible

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// TestRenderInventoryNamesHostsByTheRecordNameAndNotTheAddress is the whole
// reason this is a file rather than ansible's inline `-i "probe.invalid:22,"`
// form. Measured against ansible 2.16.3, the inline form names the host by
// its address, which breaks --limit and makes inventory_hostname an IP.
func TestRenderInventoryNamesHostsByTheRecordNameAndNotTheAddress(t *testing.T) {
	out, err := renderInventory([]Target{{Name: "dev-agent", Address: "dev-agent.invalid:22"}})
	if err != nil {
		t.Fatalf("renderInventory: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "    dev-agent:\n") {
		t.Fatalf("host is not keyed by its record name:\n%s", got)
	}
	if !strings.Contains(got, `ansible_host: "dev-agent.invalid"`) {
		t.Fatalf("address is not ansible_host:\n%s", got)
	}
	if !strings.Contains(got, "ansible_port: 22\n") {
		t.Fatalf("port was not split out of the address:\n%s", got)
	}
	if strings.Contains(got, "    dev-agent.invalid:\n") {
		t.Fatalf("host is keyed by its address, which is the inline-inventory bug:\n%s", got)
	}
}

// TestRenderInventoryIsPerHostAndNotGlobal is the OTHER inline form's bug:
// `-i "a,b," -e ansible_host=X` gives BOTH hosts X. Measured at 2.16.3:
// "alpha -> probe.invalid" and "dev-agent -> probe.invalid". Correct at one host and
// silently wrong at two.
func TestRenderInventoryIsPerHostAndNotGlobal(t *testing.T) {
	out, err := renderInventory([]Target{
		{Name: "alpha", Address: "alpha.invalid", User: "deploy"},
		{Name: "beta", Address: "beta.invalid:2222", User: "root"},
	})
	if err != nil {
		t.Fatalf("renderInventory: %v", err)
	}
	got := string(out)
	for _, want := range []string{`ansible_host: "alpha.invalid"`, `ansible_host: "beta.invalid"`,
		`ansible_user: "deploy"`, `ansible_user: "root"`, "ansible_port: 2222"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q, so the two hosts did not get their own values:\n%s", want, got)
		}
	}
	if strings.Count(got, "ansible_host:") != 2 || strings.Count(got, "ansible_user:") != 2 {
		t.Fatalf("expected one ansible_host and one ansible_user per host:\n%s", got)
	}
}

// TestRenderInventoryOmitsWhatWasNotStated pins that an unstated address or
// user writes NO line, rather than an empty one. An empty ansible_host is
// not "connect to the name", it is a connection to nowhere.
func TestRenderInventoryOmitsWhatWasNotStated(t *testing.T) {
	out, err := renderInventory([]Target{{Name: "tailnet-box"}})
	if err != nil {
		t.Fatalf("renderInventory: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "ansible_host") || strings.Contains(got, "ansible_user") || strings.Contains(got, "ansible_port") {
		t.Fatalf("an unstated field was written anyway:\n%s", got)
	}
	if !strings.Contains(got, "    tailnet-box:\n") {
		t.Fatalf("the host itself is missing:\n%s", got)
	}
}

// TestRenderInventoryTreatsABareIPv6LiteralAsAHost pins the reason
// splitAddress does not simply trust net.SplitHostPort: "::1" fails it with
// "too many colons", and reading that as a malformed address would refuse a
// perfectly good one.
func TestRenderInventoryTreatsABareIPv6LiteralAsAHost(t *testing.T) {
	for _, tc := range []struct{ addr, host, port string }{
		{"::1", "::1", ""},
		{"[::1]:22", "::1", "22"},
		{"fd00::5", "fd00::5", ""},
	} {
		host, port, err := splitAddress(tc.addr)
		if err != nil {
			t.Fatalf("splitAddress(%q): %v", tc.addr, err)
		}
		if host != tc.host || port != tc.port {
			t.Fatalf("splitAddress(%q) = %q,%q; want %q,%q", tc.addr, host, port, tc.host, tc.port)
		}
	}
}

// TestRenderInventoryRefusesAValueThatWouldBecOmeAnotherLine is why
// checkToken exists. A login carrying a newline does not produce a bad
// user: it produces an ADDITIONAL LINE in this file, setting a variable
// nobody wrote.
func TestRenderInventoryRefusesAValueThatWouldBecomeAnotherLine(t *testing.T) {
	for _, bad := range []Target{
		{Name: "a", User: "root\n      ansible_host: \"evil\""},
		{Name: "a\n    b", Address: "alpha.invalid"},
		{Name: "a", Address: "alpha.invalid", Groups: []string{"g\n    h"}},
	} {
		if _, err := renderInventory([]Target{bad}); err == nil {
			t.Fatalf("rendered a target that would inject a line: %+v", bad)
		}
	}
}

func TestRenderInventoryRefusesNoTargetsAndDuplicateNames(t *testing.T) {
	if _, err := renderInventory(nil); err == nil {
		t.Fatal("rendered an inventory with no targets")
	}
	_, err := renderInventory([]Target{{Name: "a", Address: "alpha.invalid"}, {Name: "a", Address: "beta.invalid"}})
	if err == nil {
		t.Fatal("rendered one name twice, so one address silently won")
	}
}

// TestRenderInventoryGroupsHostsByTheirDeclaredGroups covers what --limit
// cannot do: make group_vars/<role>/ resolve.
func TestRenderInventoryGroupsHostsByTheirDeclaredGroups(t *testing.T) {
	out, err := renderInventory([]Target{
		{Name: "beta", Address: "beta.invalid", Groups: []string{"dev-workstation"}},
		{Name: "alpha", Address: "alpha.invalid", Groups: []string{"dev-workstation", "hub"}},
	})
	if err != nil {
		t.Fatalf("renderInventory: %v", err)
	}
	got := string(out)
	want := "  children:\n    dev-workstation:\n      hosts:\n        alpha:\n        beta:\n    hub:\n      hosts:\n        alpha:\n"
	if !strings.Contains(got, want) {
		t.Fatalf("groups are not rendered as children of all:\nwant substring:\n%s\ngot:\n%s", want, got)
	}
}

// TestRenderInventoryIsDeterministic pins that the file is a function of
// the targets and not of map iteration or argument order -- two passes over
// one commit must build the same bytes.
func TestRenderInventoryIsDeterministic(t *testing.T) {
	a := []Target{
		{Name: "beta", Address: "beta.invalid", Groups: []string{"hub", "dev"}},
		{Name: "alpha", Address: "alpha.invalid", Groups: []string{"dev", "hub"}},
	}
	b := []Target{a[1], a[0]}
	first, err := renderInventory(a)
	if err != nil {
		t.Fatalf("renderInventory: %v", err)
	}
	second, err := renderInventory(b)
	if err != nil {
		t.Fatalf("renderInventory: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("argument order changed the file:\n%s\n---\n%s", first, second)
	}
}

// TestRunPassesTheGeneratedInventoryToAnsible is the regression for
// tonight's failure, which was a WARNING and not an error: with no -i,
// ansible parsed no inventory, found only the implicit localhost, and
// reported "Could not match supplied host pattern, ignoring: dev-agent"
// while going on to report a run.
func TestRunPassesTheGeneratedInventoryToAnsible(t *testing.T) {
	bin, argvFile, _, invFile := fakeAnsiblePlaybookWithInventory(t, `printf '%s' '`+jsonCallback+`'`)
	dir := newPlay(t)
	_, err := (Runner{Bin: bin, Stderr: io.Discard}).Apply(context.Background(), dir,
		[]Target{{Name: "dev-agent", Address: "dev-agent.invalid:22", User: "root", Groups: []string{"dev-workstation"}}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading argv: %v", err)
	}
	if !strings.Contains(string(argv), "-i\n") {
		t.Fatalf("no -i in argv, so ansible parsed no inventory:\n%s", argv)
	}
	inv, err := os.ReadFile(invFile)
	if err != nil {
		t.Fatalf("the inventory the child was given was not readable: %v", err)
	}
	for _, want := range []string{"    dev-agent:\n", `ansible_host: "dev-agent.invalid"`, "ansible_port: 22", `ansible_user: "root"`, "dev-workstation:"} {
		if !strings.Contains(string(inv), want) {
			t.Fatalf("the child's inventory is missing %q:\n%s", want, inv)
		}
	}
}

// TestRunRemovesTheInventoryAfterwards pins that the file naming every
// machine in the fleet and the account each is reached as does not outlive
// the run that needed it.
func TestRunRemovesTheInventoryAfterwards(t *testing.T) {
	bin, argvFile, _, _ := fakeAnsiblePlaybookWithInventory(t, `printf '%s' '`+jsonCallback+`'`)
	dir := newPlay(t)
	if _, err := (Runner{Bin: bin, Stderr: io.Discard}).Apply(context.Background(), dir,
		[]Target{{Name: "dev-agent", Address: "alpha.invalid"}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("reading argv: %v", err)
	}
	path := argvLines(string(argv))[2]
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("the generated inventory %s still exists after the run", path)
	}
}

// TestRunAlwaysPinsTheGroupNameTransform is the second half of the
// can't-be-weakened rule the JSON callback already follows, and it protects
// something subtler: a renamed group does not fail, it silently resolves
// different group_vars. Measured at ansible 2.16.3, "always" turns
// "dev-workstation" into "dev_workstation" with no warning.
func TestRunAlwaysPinsTheGroupNameTransform(t *testing.T) {
	bin, _, envFile, _ := fakeAnsiblePlaybookWithInventory(t, `printf '%s' '`+jsonCallback+`'`)
	r := Runner{Bin: bin, Env: []string{"ANSIBLE_TRANSFORM_INVALID_GROUP_CHARS=always"}, Stderr: io.Discard}
	if _, err := r.Apply(context.Background(), newPlay(t),
		[]Target{{Name: "dev-agent", Address: "alpha.invalid", Groups: []string{"dev-workstation"}}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("reading env: %v", err)
	}
	lines := strings.Split(string(env), "\n")
	last := ""
	for _, l := range lines {
		if strings.HasPrefix(l, "ANSIBLE_TRANSFORM_INVALID_GROUP_CHARS=") {
			last = l
		}
	}
	if last != "ANSIBLE_TRANSFORM_INVALID_GROUP_CHARS=never" {
		t.Fatalf("effective setting is %q, so a caller can rename our groups", last)
	}
}
