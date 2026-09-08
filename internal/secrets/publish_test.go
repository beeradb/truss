package secrets

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newPublishTestKV is newTestKV's write-path counterpart: it builds a KV
// against a fresh fakeVault, constructed to write only the named item, the
// same way a real publish pass constructs one KV per credential it mints.
func newPublishTestKV(t *testing.T, fv *fakeVault, srv *httptest.Server, writable string) *KV {
	t.Helper()
	jwt := jwtFixture()
	jwtPath := writeJWTFixture(t, jwt)

	kv, err := NewKV(KVConfig{
		Addr:         srv.URL,
		Mount:        "platform",
		Role:         "applier",
		JWTPath:      jwtPath,
		HTTP:         srv.Client(),
		WritableItem: writable,
	})
	if err != nil {
		t.Fatalf("NewKV: %v", err)
	}
	return kv
}

func TestPatchExpirySendsAMergePatchAndTouchesNoDataPath(t *testing.T) {
	fv := newFakeVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	if err := kv.PatchExpiry(context.Background(), "cf-infra-admin", "2027-01-01"); err != nil {
		t.Fatalf("PatchExpiry: %v", err)
	}

	found := false
	for _, req := range fv.recordedRequests() {
		if req.Method == http.MethodPatch && strings.Contains(req.Path, "/metadata/cf-infra-admin") {
			found = true
			if req.ContentType != "application/merge-patch+json" {
				t.Errorf("PATCH content-type = %q, want application/merge-patch+json", req.ContentType)
			}
			if !strings.Contains(req.Body, `"expires":"2027-01-01"`) {
				t.Errorf("PATCH body = %q, want it to carry custom_metadata.expires", req.Body)
			}
		}
		if strings.Contains(req.Path, "/data/") {
			t.Errorf("PatchExpiry touched a /data/ path: %+v", req)
		}
	}
	if !found {
		t.Fatal("no PATCH request reached the metadata path")
	}
}

func TestPatchExpiryNeverPostsToTheMetadataEndpoint(t *testing.T) {
	fv := newFakeVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	if err := kv.PatchExpiry(context.Background(), "cf-infra-admin", "2027-01-01"); err != nil {
		t.Fatalf("PatchExpiry: %v", err)
	}

	for _, req := range fv.recordedRequests() {
		if req.Method == http.MethodPost && strings.Contains(req.Path, "/metadata/") {
			t.Errorf("a POST reached the metadata endpoint: %+v -- POST resets max_versions, cas_required and delete_version_after, and a merge PATCH is required instead", req)
		}
	}
}

// TestPatchExpirySurfacesAMissingPatchCapabilityLegibly is the §13.2
// unverified case made concrete, and the coordinator's follow-up on top of
// it: a 403 (policy lacks the "patch" capability -- expected today, pending
// a separate owner decision) and a 405 (the KV plugin cannot answer PATCH
// at all) are different problems for different people, and the message
// must not conflate them. Where the status genuinely cannot tell the two
// apart, the message must say so rather than guessing.
func TestPatchExpirySurfacesAMissingPatchCapabilityLegibly(t *testing.T) {
	cases := []struct {
		status      int
		wantMention []string // substrings the message must contain
		wantAbsent  []string // substrings it must NOT contain -- the other cause
	}{
		{
			status:      http.StatusForbidden,
			wantMention: []string{"403", "policy", "patch"},
			// The message may name "KV plugin" only to rule it out
			// ("not a KV plugin problem"); what it must never do is claim
			// the plugin is too old, which is the 405 diagnosis.
			wantAbsent: []string{"too old"},
		},
		{
			status:      http.StatusMethodNotAllowed,
			wantMention: []string{"KV plugin", "too old"},
			// May name "policy" only to rule it out ("not a policy
			// problem"); must never claim the policy lacks the grant.
			wantAbsent: []string{"does not grant"},
		},
		{
			status:      http.StatusNotImplemented,
			wantMention: []string{"KV plugin", "too old"},
			wantAbsent:  []string{"does not grant"},
		},
		{
			// A status that says nothing about which cause it is: the
			// message must name both possibilities and admit it cannot
			// tell them apart, rather than pointing at just one.
			status:      http.StatusInternalServerError,
			wantMention: []string{"policy", "KV plugin", "does not say which"},
		},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			fv := newFakeVault()
			fv.patchStatus["cf-infra-admin"] = tc.status
			srv := fv.server()
			defer srv.Close()
			kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

			err := kv.PatchExpiry(context.Background(), "cf-infra-admin", "2027-01-01")
			if err == nil {
				t.Fatal("PatchExpiry against a server refusing PATCH = nil error")
			}
			msg := err.Error()
			for _, want := range tc.wantMention {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(msg, absent) {
					t.Errorf("error %q wrongly points at %q, conflating the two causes", msg, absent)
				}
			}
		})
	}
}

func TestPatchExpiryCannotCreateAnItem(t *testing.T) {
	// A PATCH to metadata for a title that has never had a version written
	// 404s in real Vault, and the fake's default (unknown item -> nothing
	// in f.meta, but PATCH still succeeds and would populate f.meta) does
	// not model that by itself, so this only asserts what this package's
	// contract requires: a PATCH that is refused must be reported as an
	// error, never silently treated as having created the item.
	fv := newFakeVault()
	fv.patchStatus["never-minted"] = http.StatusNotFound
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "never-minted")

	if err := kv.PatchExpiry(context.Background(), "never-minted", "2027-01-01"); err == nil {
		t.Fatal("PatchExpiry against a 404 (no such item) = nil error, want a refusal")
	}
}

func TestPutValueRefusesAnyItemButTheMintedOne(t *testing.T) {
	fv := newFakeVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	err := kv.PutValue(context.Background(), "some-other-item", map[string]string{"password": "x"}, 0)
	if err == nil {
		t.Fatal("PutValue on an item other than WritableItem = nil error, want a refusal")
	}
	if _, wrote := fv.dataWriteFor("some-other-item"); wrote {
		t.Error("PutValue on a refused item still reached the server")
	}
}

func TestPutValueRefusesWhenConstructedWithNoWritableItem(t *testing.T) {
	fv := newFakeVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "") // read-only construction

	if err := kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{"password": "x"}, 0); err == nil {
		t.Fatal("PutValue on a KV built with no WritableItem = nil error, want a refusal")
	}
}

func TestPutValueRefusesNoFieldsAtAll(t *testing.T) {
	fv := newFakeVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	if err := kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{}, 0); err == nil {
		t.Fatal("PutValue with no fields at all = nil error, want a refusal")
	}
	if _, wrote := fv.dataWriteFor("cf-infra-admin"); wrote {
		t.Error("a fieldless write still reached the server")
	}
}

func TestPutValueRefusesAnEmptyOrWhitespaceValue(t *testing.T) {
	fv := newFakeVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	cases := map[string]string{
		"empty":      "",
		"whitespace": "   \t\n",
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			err := kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{"password": v}, 0)
			if err == nil {
				t.Fatalf("PutValue with a %s value = nil error, want a refusal", name)
			}
			if _, wrote := fv.dataWriteFor("cf-infra-admin"); wrote {
				t.Errorf("a %s value still reached the server", name)
			}
		})
	}
}

func TestPutValueSendsCASAndDoesNotRetryOnMismatch(t *testing.T) {
	fv := newFakeVault()
	fv.setDataCAS("cf-infra-admin", 5) // a concurrent writer already moved the version to 5
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	err := kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{"password": "gen2-token"}, 3) // stale cas
	if err == nil {
		t.Fatal("PutValue with a stale cas = nil error, want a refusal")
	}

	dataPosts := 0
	for _, req := range fv.recordedRequests() {
		if req.Method == http.MethodPost && strings.Contains(req.Path, "/data/cf-infra-admin") {
			dataPosts++
		}
	}
	if dataPosts != 1 {
		t.Errorf("PutValue on a cas mismatch made %d POST(s) to /data/, want exactly 1 -- a mismatch must never be retried", dataPosts)
	}
	if _, wrote := fv.dataWriteFor("cf-infra-admin"); wrote {
		t.Error("a cas-mismatched write still landed in the store")
	}
}

func TestPutValueSucceedsWithTheCorrectCAS(t *testing.T) {
	fv := newFakeVault()
	fv.setDataCAS("cf-infra-admin", 5)
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	if err := kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{"password": "gen2-token"}, 5); err != nil {
		t.Fatalf("PutValue with the correct cas = %v, want no error", err)
	}
	written, ok := fv.dataWriteFor("cf-infra-admin")
	if !ok || written["password"] != "gen2-token" {
		t.Errorf("dataWriteFor(cf-infra-admin) = (%v, %v), want the written value recorded", written, ok)
	}
}

// TestTheMintedValueNeverAppearsInAPublishError is the redaction proof: the
// fake echoes the exact request body back into a 500 response
// (handlePutData's status!=0 branch), so if PutValue failed to redact the
// value it sent, this test would see it.
func TestTheMintedValueNeverAppearsInAPublishError(t *testing.T) {
	const sentinel = "SENTINEL-CF-INFRA-ADMIN-VALUE-DO-NOT-LEAK"
	fv := newFakeVault()
	fv.putStatus["cf-infra-admin"] = 500
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	err := kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{"password": sentinel}, 0)
	if err == nil {
		t.Fatal("PutValue against a 500 = nil error, want an error to inspect")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error %q contains the value being written", err.Error())
	}
}

func TestTheClientTokenAndTheJWTStillNeverAppearInAPublishError(t *testing.T) {
	fv := newFakeVault()
	fv.loginReply = authReplyToken()
	fv.putStatus["cf-infra-admin"] = 500
	srv := fv.server()
	defer srv.Close()

	jwt := jwtFixture()
	jwtPath := writeJWTFixture(t, jwt)
	kv, err := NewKV(KVConfig{
		Addr: srv.URL, Mount: "platform", Role: "applier", JWTPath: jwtPath, HTTP: srv.Client(),
		WritableItem: "cf-infra-admin",
	})
	if err != nil {
		t.Fatalf("NewKV: %v", err)
	}

	err = kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{"password": "some-token-value"}, 0)
	if err == nil {
		t.Fatal("PutValue against a 500 = nil error, want an error to inspect")
	}
	msg := err.Error()
	if strings.Contains(msg, fv.loginReply) {
		t.Errorf("error %q contains the client token", msg)
	}
	if strings.Contains(msg, jwt) {
		t.Errorf("error %q contains the JWT", msg)
	}
}

func TestNoRequestIsEverMadeToAVaultDeleteOrDestroyPath(t *testing.T) {
	fv := newFakeVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv, "cf-infra-admin")

	if err := kv.PatchExpiry(context.Background(), "cf-infra-admin", "2027-01-01"); err != nil {
		t.Fatalf("PatchExpiry: %v", err)
	}
	if err := kv.PutValue(context.Background(), "cf-infra-admin", map[string]string{"password": "gen1-token"}, 0); err != nil {
		t.Fatalf("PutValue: %v", err)
	}

	for _, req := range fv.recordedRequests() {
		if strings.Contains(req.Path, "/delete/") || strings.Contains(req.Path, "/destroy/") || strings.Contains(req.Path, "/undelete/") {
			t.Errorf("a request reached a delete/destroy/undelete path: %+v -- Publisher must never be able to remove a version", req)
		}
	}
}

// TestTrussNeverExecsAVaultBinary enforces this package's own doc.go claim
// ("it never shells out") with a static check rather than a comment: an AST
// scan of every .go file in this directory (this package owns the entire
// Vault HTTP conversation, read and write) for any exec.Command or
// exec.CommandContext call at all. vault/bootstrap-vault.sh already names
// why argv is refused -- visible in ps, in /proc, and in the exec request
// URI that lands in an audit log -- and TestSecretsImportsOnlyTheStandardLibrary
// would not by itself catch a hidden call to os/exec, since os/exec is
// itself the standard library.
//
// ⚠️ This is belt and braces, not the only guard: probed directly on the
// running truss image (localhost/truss:port-20260908a) on 2026-09-08, it
// carries no `vault` binary at all -- tofu, git and op are all present,
// vault is not -- so today the rule is also structurally enforced by the
// image simply having nothing to exec. That is an accident of the current
// base image, not a property of this source; the Dockerfile's base is a
// build arg and could change under a future edit that never touches this
// package. This test is what keeps the rule true regardless of what any
// particular image happens to contain.
//
// ⚠️ opstore.go IS EXCLUDED, DELIBERATELY, AND THE RULE THIS TEST POLICES
// DID NOT CHANGE. This package now covers a second Store, OP, backed by
// the `op` CLI rather than an HTTP API -- 1Password has no equivalent to
// Vault's HTTP surface for a service account to speak directly, so exec is
// genuinely how it is used, the same way apply.sh always called `op` as a
// binary rather than hand-rolling its wire protocol. That is a fact about
// a DIFFERENT credential store than the one this test's name and message
// are about. The claim this test still enforces -- every OTHER file in
// this package never shells out to anything, and nothing here ever execs a
// binary literally named "vault" -- is unchanged; see
// TestOPsExecTargetIsConfigNeverALiteral (opstore_test.go), which pins that
// opstore.go's one exec.CommandContext call always names cfg.Bin (a config
// field NewOP defaults to "op") and never a literal string of any kind, let
// alone "vault".
func TestTrussNeverExecsAVaultBinary(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if entry.Name() == "opstore.go" {
			continue
		}
		checked++
		path := filepath.Join(dir, entry.Name())
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "exec" {
				return true
			}
			t.Errorf("%s calls exec.%s -- this package speaks Vault's HTTP API directly and must never shell out to a vault binary", entry.Name(), sel.Sel.Name)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no .go files found in the current directory -- test is not checking anything")
	}
}
