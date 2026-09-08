package secrets

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeOP is an injectable `op` process: a table of canned responses keyed
// by the joined argv, so a test can arrange exactly what one `item list`
// or `op read` call answers without ever starting a real subprocess.
type fakeOP struct {
	calls [][]string
	envs  [][]string

	// responses maps a joined argv ("item list --vault …") to a canned
	// reply. A call with no matching entry fails the test outright rather
	// than silently returning zero values, so a forgotten arrangement
	// shows up as a broken test rather than a wrongly-passing one.
	responses map[string]fakeOPResponse
	t         *testing.T
}

type fakeOPResponse struct {
	stdout string
	stderr string
	err    error // non-nil simulates a non-zero exit (or a failure to start)
}

func newFakeOP(t *testing.T) *fakeOP {
	return &fakeOP{responses: map[string]fakeOPResponse{}, t: t}
}

func (f *fakeOP) arrange(resp fakeOPResponse, args ...string) {
	f.responses[strings.Join(args, "\x00")] = resp
}

func (f *fakeOP) run(ctx context.Context, env []string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	f.envs = append(f.envs, append([]string(nil), env...))
	key := strings.Join(args, "\x00")
	resp, ok := f.responses[key]
	if !ok {
		f.t.Fatalf("fakeOP: no response arranged for op %s", strings.Join(args, " "))
	}
	return []byte(resp.stdout), []byte(resp.stderr), resp.err
}

// exitErr stands in for the *exec.ExitError a real `op` failure returns --
// fakeOPResponse.err only needs to be non-nil, since OP.exec never
// inspects its type, only whether it is nil.
var exitErr = errors.New("exit status 1")

func writeOPToken(t *testing.T, value string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("writing token fixture: %v", err)
	}
	return path
}

func newTestOP(t *testing.T, f *fakeOP, vault string) *OP {
	t.Helper()
	op, err := NewOP(OPConfig{
		Vault:     vault,
		TokenFile: writeOPToken(t, opTokenFixture()),
	})
	if err != nil {
		t.Fatalf("NewOP: %v", err)
	}
	op.run = f.run
	return op
}

// opTokenFixture assembles a token-shaped string from parts rather than
// one literal, matching jwtFixture's reasoning: it should not itself read
// as a credential to a scanner over this fixture file.
func opTokenFixture() string {
	parts := []string{"ops", "svcacct", "faketoken", "0001"}
	return strings.Join(parts, "-")
}

func TestNewOPRefusesAnEmptyVaultOrTokenFile(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("tok"), 0o600); err != nil {
		t.Fatalf("writing token fixture: %v", err)
	}
	base := OPConfig{Vault: "recipes-runtime", TokenFile: tokenPath}

	cases := []struct {
		name string
		cfg  OPConfig
	}{
		{"empty Vault", func() OPConfig { c := base; c.Vault = ""; return c }()},
		{"empty TokenFile", func() OPConfig { c := base; c.TokenFile = ""; return c }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewOP(tc.cfg); err == nil {
				t.Fatalf("NewOP(%+v) = nil error, want a refusal", tc.cfg)
			}
		})
	}

	if _, err := NewOP(base); err != nil {
		t.Fatalf("NewOP with every field set = %v, want no error", err)
	}
}

func TestOPListRefusesAFailedListRatherThanReportingItEmpty(t *testing.T) {
	// ⚠️ THIS IS THE ASSERTION THAT MATTERS MOST. The deployed bash lost
	// exactly this failure to a subshell (mapfile < <(OP item list ...)):
	// a rate-limited or otherwise-failed list produced zero titles, the
	// loop that would have noticed never ran, and the pass reported a
	// clean sweep over a question it never got to ask.
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stderr: "Too many requests. Your client has been rate-limited.", err: exitErr},
		"item", "list", "--vault", "recipes-runtime", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")

	items, err := op.List(context.Background())
	if err == nil {
		t.Fatalf("List on a failed op invocation = (%v, nil), want an error", items)
	}
	if items != nil {
		t.Errorf("List on a failure returned %v, want nil", items)
	}
}

func TestOPListOnAGenuinelyEmptyVaultIsNotAnError(t *testing.T) {
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stdout: "[]"},
		"item", "list", "--vault", "recipes-runtime", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")

	items, err := op.List(context.Background())
	if err != nil {
		t.Fatalf("List on a genuinely empty vault = %v, want no error", err)
	}
	if len(items) != 0 {
		t.Errorf("List on an empty vault = %v, want none", items)
	}
}

func TestOPListReturnsEveryTitle(t *testing.T) {
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stdout: `[{"title":"cloudflare_api_token.runtime"},{"title":"cloudflare-images-token"}]`},
		"item", "list", "--vault", "recipes-runtime", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")

	items, err := op.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"cloudflare_api_token.runtime", "cloudflare-images-token"}
	if !reflect.DeepEqual(items, want) {
		t.Errorf("List = %v, want %v", items, want)
	}
}

func TestOPExpiryOnAMissingFieldIsUnrecordedNotAnError(t *testing.T) {
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stderr: `[ERROR] "expires" doesn't have a value under item "some-bot-token"`, err: exitErr},
		"item", "get", "some-bot-token", "--vault", "recipes-runtime", "--fields", "expires", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")

	raw, recorded, err := op.Expiry(context.Background(), "some-bot-token")
	if err != nil {
		t.Fatalf("Expiry on a missing field = (%q, %v, %v), want no error", raw, recorded, err)
	}
	if recorded {
		t.Errorf("Expiry on a missing field reported recorded=true")
	}
	if raw != "" {
		t.Errorf("Expiry on a missing field returned raw=%q, want empty", raw)
	}
}

func TestOPExpiryOnAGenuineFailureIsAnErrorNeverUnrecorded(t *testing.T) {
	// ⚠️ THE OTHER DIRECTION OF THE SAME MISTAKE. Treating a rate limit or
	// a bad token as "this item just has no expires field" is the vacuous
	// pass again, one item at a time instead of for the whole vault.
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stderr: "Too many requests. Your client has been rate-limited.", err: exitErr},
		"item", "get", "cloudflare_api_token.runtime", "--vault", "recipes-runtime", "--fields", "expires", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")

	raw, recorded, err := op.Expiry(context.Background(), "cloudflare_api_token.runtime")
	if err == nil {
		t.Fatalf("Expiry on a rate limit = (%q, %v, nil), want an error", raw, recorded)
	}
	if recorded {
		t.Errorf("Expiry on a genuine failure reported recorded=true")
	}
}

func TestOPExpiryReturnsTheRecordedValue(t *testing.T) {
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stdout: `[{"label":"expires","value":"2026-11-30"}]`},
		"item", "get", "cloudflare_api_token.runtime", "--vault", "recipes-runtime", "--fields", "expires", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")

	raw, recorded, err := op.Expiry(context.Background(), "cloudflare_api_token.runtime")
	if err != nil {
		t.Fatalf("Expiry: %v", err)
	}
	if !recorded || raw != "2026-11-30" {
		t.Fatalf("Expiry = (%q, %v), want (\"2026-11-30\", true)", raw, recorded)
	}
}

func TestOPExpiryNeverPasses(t *testing.T) {
	// Sweep.Run is what actually treats "never" as an opt-out (skips it
	// entirely); this only pins that OP.Expiry hands the literal value
	// through unchanged rather than doing anything special with it.
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stdout: `[{"label":"expires","value":"never"}]`},
		"item", "get", "telegram-bot-token", "--vault", "recipes-runtime", "--fields", "expires", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")

	raw, recorded, err := op.Expiry(context.Background(), "telegram-bot-token")
	if err != nil {
		t.Fatalf("Expiry: %v", err)
	}
	if !recorded || raw != "never" {
		t.Fatalf("Expiry = (%q, %v), want (\"never\", true)", raw, recorded)
	}
}

func TestOPNeverPutsTheTokenInArgv(t *testing.T) {
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stdout: "[]"},
		"item", "list", "--vault", "recipes-runtime", "--format", "json")
	op := newTestOP(t, f, "recipes-runtime")
	tokenPath := op.cfg.TokenFile
	tok, err := (&OP{cfg: OPConfig{TokenFile: tokenPath}}).token()
	if err != nil {
		t.Fatalf("reading back the token fixture: %v", err)
	}

	if _, err := op.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}

	for _, call := range f.calls {
		for _, arg := range call {
			if strings.Contains(arg, tok) {
				t.Fatalf("token appeared in argv: %v", call)
			}
		}
	}
	// The token DOES belong in the environment -- that is how `op`
	// authenticates -- so assert it is there, not merely absent from argv.
	found := false
	for _, env := range f.envs {
		for _, kv := range env {
			if kv == "OP_SERVICE_ACCOUNT_TOKEN="+tok {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("OP_SERVICE_ACCOUNT_TOKEN was never set in the subprocess environment")
	}
}

func TestOPTokenNeverAppearsInAnError(t *testing.T) {
	f := newFakeOP(t)
	op := newTestOP(t, f, "recipes-runtime")
	tok, err := (&OP{cfg: OPConfig{TokenFile: op.cfg.TokenFile}}).token()
	if err != nil {
		t.Fatalf("reading back the token fixture: %v", err)
	}
	f.arrange(fakeOPResponse{stderr: "denied for token " + tok, err: exitErr},
		"item", "list", "--vault", "recipes-runtime", "--format", "json")

	_, err = op.List(context.Background())
	if err == nil {
		t.Fatal("List with a failing fake = nil error, want an error to inspect")
	}
	if strings.Contains(err.Error(), tok) {
		t.Errorf("error %q contains the service-account token", err.Error())
	}
}

func TestNoVaultOrTokenPathIsHardcodedInOP(t *testing.T) {
	f := newFakeOP(t)
	f.arrange(fakeOPResponse{stdout: `[{"title":"an-unusual-item-name"}]`},
		"item", "list", "--vault", "a-very-unusual-vault-name", "--format", "json")
	f.arrange(fakeOPResponse{stdout: `[{"label":"expires","value":"never"}]`},
		"item", "get", "an-unusual-item-name", "--vault", "a-very-unusual-vault-name", "--fields", "expires", "--format", "json")
	op := newTestOP(t, f, "a-very-unusual-vault-name")

	if _, err := op.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, _, err := op.Expiry(context.Background(), "an-unusual-item-name"); err != nil {
		t.Fatalf("Expiry: %v", err)
	}

	found := false
	for _, call := range f.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "a-very-unusual-vault-name") {
			found = true
		}
	}
	if !found {
		t.Error("no op invocation carried the configured vault name -- something is hardcoded")
	}
}

// TestOPDefaultsItsBinaryButNeverItsVaultOrToken pins that the only
// implicit default OP carries is the "op" binary name -- resolved via
// PATH, matching the bash's own `op` invocation -- and that Vault and
// TokenFile are never defaulted.
func TestOPDefaultsItsBinaryButNeverItsVaultOrToken(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenPath, []byte("tok"), 0o600); err != nil {
		t.Fatalf("writing token fixture: %v", err)
	}
	op, err := NewOP(OPConfig{Vault: "recipes-runtime", TokenFile: tokenPath})
	if err != nil {
		t.Fatalf("NewOP: %v", err)
	}
	if op.cfg.Bin != "op" {
		t.Errorf("Bin defaulted to %q, want \"op\"", op.cfg.Bin)
	}
}

// TestOPsExecTargetIsConfigNeverALiteral is the counterpart to
// TestTrussNeverExecsAVaultBinary's exclusion of opstore.go from that
// test's file scan (publish_test.go): that test can no longer ban
// exec.CommandContext outright across this whole package now that OP's
// entire job is running the `op` CLI, so this pins down, statically, the
// property the ban actually protected -- that the binary opstore.go execs
// is always cfg.Bin, a config field (NewOP defaults it to "op",
// TestOPDefaultsItsBinaryButNeverItsVaultOrToken above covers that), and
// never a string literal baked into the call site. A hardcoded
// exec.CommandContext(ctx, "vault", ...) anywhere in this file would fail
// this test, whether or not it was ever reachable.
func TestOPsExecTargetIsConfigNeverALiteral(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	path := filepath.Join(dir, "opstore.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing opstore.go: %v", err)
	}

	found := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "CommandContext" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "exec" {
			return true
		}
		found++
		if len(call.Args) < 2 {
			t.Fatalf("exec.CommandContext call in opstore.go has fewer than 2 arguments")
			return false
		}
		binSel, ok := call.Args[1].(*ast.SelectorExpr)
		if !ok {
			t.Fatalf("exec.CommandContext's binary argument in opstore.go is %T, want a selector expression reading a config field, never a literal", call.Args[1])
			return false
		}
		if binSel.Sel.Name != "Bin" {
			t.Fatalf("exec.CommandContext's binary argument in opstore.go is field %q, want Bin", binSel.Sel.Name)
		}
		return true
	})
	if found != 1 {
		t.Fatalf("found %d exec.CommandContext call(s) in opstore.go, want exactly 1", found)
	}
}
