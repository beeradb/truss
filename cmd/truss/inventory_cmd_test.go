package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validInventoryFiles is the smallest inventory internal/inventory.Load
// reads and Check accepts, copied from internal/inventory/load_test.go's
// validTree() -- this command uses os.DirFS, which needs real files on
// disk, unlike that package's own tests which build an fstest.MapFS in
// memory.
func validInventoryFiles() map[string]string {
	return map[string]string{
		"inventory/hosts/alpha.json": `{
			"schema":"truss.host/v1","name":"alpha","kind":"vm","role":"k8s-node",
			"provisioned_by":"hosts/alpha","config":"ansible/alpha",
			"tailnet_tags":["k8s"],"cluster":"prod","frozen":false,"decommissioned":false}`,
		"inventory/clusters/prod.json": `{
			"schema":"truss.cluster/v1","name":"prod","distribution":"k3s",
			"hosts":["alpha"],"baseline":"baselines/prod","capabilities":["ingress"],
			"kubeconfig_item":"prod-kubeconfig","has_ha_vault":true}`,
		"inventory/projects/wren.json": `{
			"schema":"truss.project/v1","name":"wren","environments":["prod"]}`,
		"inventory/environments/wren/prod.json": `{
			"schema":"truss.environment/v1","project":"wren","name":"prod",
			"shape":"kubernetes","placement":{"cluster":"prod","namespace":"web"},
			"requires":["ingress"],"vault":{"mount":"secret","prefix":"wren/prod"},
			"frozen":false}`,
		"deliveries/prod/web/kustomization.yaml": "kind: Kustomization\n",
	}
}

// writeInventoryTree materialises files (relative path -> content) under
// root, creating parent directories as needed.
func writeInventoryTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func TestInventoryValidateAcceptsAValidTree(t *testing.T) {
	dir := t.TempDir()
	writeInventoryTree(t, dir, validInventoryFiles())

	var stdout, stderr bytes.Buffer
	code := run([]string{"inventory", "validate", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %q stderr: %q)", code, stdout.String(), stderr.String())
	}
}

// TestInventoryValidateReportsADanglingClusterReference points a host at a
// cluster record that does not exist. inventory.Check's own message names
// the host's own path, which is what a person fixing the tree needs to find
// the record.
func TestInventoryValidateReportsADanglingClusterReference(t *testing.T) {
	dir := t.TempDir()
	files := validInventoryFiles()
	files["inventory/hosts/alpha.json"] = `{
		"schema":"truss.host/v1","name":"alpha","kind":"vm","role":"k8s-node",
		"provisioned_by":"hosts/alpha","config":"ansible/alpha",
		"tailnet_tags":["k8s"],"cluster":"ghost","frozen":false,"decommissioned":false}`
	writeInventoryTree(t, dir, files)

	var stdout, stderr bytes.Buffer
	code := run([]string{"inventory", "validate", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stdout: %q stderr: %q)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "inventory/hosts/alpha.json") {
		t.Errorf("stdout = %q, want the problem to name the file with the dangling reference", stdout.String())
	}
}

// TestInventoryValidateRefusesATreeWithNoInventoryDirectory covers the
// difference Load itself draws between "nothing declared" and "this is not
// a platform checkout" -- an absent inventory/ is a problem, not silence.
func TestInventoryValidateRefusesATreeWithNoInventoryDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"inventory", "validate", dir}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stdout: %q stderr: %q)", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "inventory/") {
		t.Errorf("stdout = %q, want it to name the missing directory", stdout.String())
	}
}

// TestInventoryValidateJSONFlagOnAValidTree checks --json emits a parseable
// empty array and keeps the exit-0 contract a human-readable success would
// otherwise have.
func TestInventoryValidateJSONFlagOnAValidTree(t *testing.T) {
	dir := t.TempDir()
	writeInventoryTree(t, dir, validInventoryFiles())

	var stdout, stderr bytes.Buffer
	code := run([]string{"inventory", "validate", dir, "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr.String())
	}
	var problems []string
	if err := json.Unmarshal(stdout.Bytes(), &problems); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, stdout.String())
	}
	if len(problems) != 0 {
		t.Errorf("problems = %v, want none", problems)
	}
}

// TestInventoryValidateJSONFlagOnAProblemTree checks --json keeps the exit-1
// contract and still emits a parseable array, listing what is wrong rather
// than one human-readable line per problem.
func TestInventoryValidateJSONFlagOnAProblemTree(t *testing.T) {
	dir := t.TempDir() // no inventory/ at all

	var stdout, stderr bytes.Buffer
	code := run([]string{"inventory", "validate", dir, "--json"}, strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (stderr: %q)", code, stderr.String())
	}
	var problems []string
	if err := json.Unmarshal(stdout.Bytes(), &problems); err != nil {
		t.Fatalf("stdout not valid JSON: %v (%q)", err, stdout.String())
	}
	if len(problems) == 0 {
		t.Errorf("problems = %v, want at least one", problems)
	}
}

// TestInventoryValidateBadVerbOrTooManyArgs covers every usage error this
// subcommand recognises: a missing verb, a wrong verb, and more than one
// directory argument.
func TestInventoryValidateBadVerbOrTooManyArgs(t *testing.T) {
	cases := [][]string{
		{"inventory"},
		{"inventory", "bogus"},
		{"inventory", "validate", "a", "b"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := run(args, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
	}
}
