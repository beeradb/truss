package parity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"

	"fmt"
	"github.com/beeradb/truss/internal/handoff"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Harness holds the built truss binary and the fake tofu/git shim. Building
// them is the expensive part and it happens once per test binary, not once
// per scenario.
type Harness struct {
	Truss string // path to the built truss binary
	Fake  string // path to the built shim, copied per scenario as tofu and git
}

// Build compiles truss and the PATH shim into dir.
//
// The binary is BUILT and EXECUTED rather than called in-process, because
// the pass's real subprocess calls (plan.Runner's argv, execGit's exit
// codes) and its real HTTP clients (SigV4, the forge decoder, Vault) are
// exactly what a whole-pass parity test is for. A harness that injected Go
// fakes would be testing runApplyPass's orchestration a second time and
// none of the boundaries the bash's own harness crosses.
func Build(dir string) (*Harness, error) {
	trussBin := filepath.Join(dir, "truss")
	if out, err := exec.Command("go", "build", "-o", trussBin, "github.com/beeradb/truss/cmd/truss").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("parity: building truss: %v\n%s", err, out)
	}

	fake := filepath.Join(dir, "fakebin")
	if out, err := exec.Command("go", "build", "-o", fake, "github.com/beeradb/truss/internal/parity/fakebin").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("parity: building fakebin: %v\n%s", err, out)
	}
	return &Harness{Truss: trussBin, Fake: fake}, nil
}

// binDirFor gives one scenario its own PATH directory holding `tofu`,
// `git` and the fixtures those two answer from. Per scenario rather than
// shared because the fixtures file lives beside the binary -- see
// fakebin's own comment for why it cannot travel in the environment.
func (h *Harness) binDirFor(root string, fixtures []byte) (string, error) {
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(binDir, "fixtures.json"), fixtures, 0o600); err != nil {
		return "", err
	}
	for _, name := range []string{"tofu", "git"} {
		dst := filepath.Join(binDir, name)
		// A hard link where the filesystem allows one; a copy otherwise,
		// because the temp root and the build directory need not share a
		// device.
		if err := os.Link(h.Fake, dst); err == nil {
			continue
		}
		b, err := os.ReadFile(h.Fake)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(dst, b, 0o755); err != nil {
			return "", err
		}
	}
	return binDir, nil
}

// Run replays one scenario against truss and reports what it produced, in
// the same shape the corpus records for the bash.
//
// now is the clock the scenario's relative dates are materialised against.
// It is the harness's own time.Now rather than a frozen instant, because
// truss reads the real clock inside the subprocess and there is no flag to
// move it; the ±1 day that costs is why day counts are compared with a
// tolerance rather than exactly (see compare.go).
func (h *Harness) Run(ctx context.Context, s Scenario, now time.Time) (Outcome, string, error) {
	root, err := os.MkdirTemp("", "parity-")
	if err != nil {
		return Outcome{}, "", err
	}
	defer os.RemoveAll(root)

	// --- the clone the pass walks -------------------------------------
	workdir := filepath.Join(root, "workdir")
	if err := os.MkdirAll(filepath.Join(workdir, ".git"), 0o755); err != nil {
		return Outcome{}, "", err
	}
	for _, r := range s.WorkdirRoots {
		if err := os.MkdirAll(filepath.Join(workdir, r), 0o755); err != nil {
			return Outcome{}, "", err
		}
		// backend.hcl exists because the real init reads it; the shim
		// does not, but its absence would be a difference from the
		// reference harness for no reason.
		if err := os.WriteFile(filepath.Join(workdir, r, "backend.hcl"), []byte("# dummy, tofu is shimmed\n"), 0o644); err != nil {
			return Outcome{}, "", err
		}
		// ⚠️ A REAL CHECKOUT HAS A LOCKFILE, AND THE PASS NOW READS IT.
		// `-lockfile=readonly` makes the committed .terraform.lock.hcl the
		// authoritative provider set, so applyOneRoot and runDrift read it to
		// decide which credentials a root needs -- a root that declares no
		// Cloudflare provider is no longer handed the account's broadest
		// Cloudflare token. Without this file every root errored, and the
		// whole corpus went red on "could not read ... .terraform.lock.hcl".
		//
		// The CONTENT is faithful rather than uniform: `platform/` really does
		// declare only the github provider -- verified against the live repo,
		// and it is exactly why it must not get a Cloudflare token. A fixture
		// that gave every root an identical lockfile would let the gate pass
		// while testing nothing, which is the "fixture more forgiving than
		// production" failure this project has already paid a day for.
		if err := os.WriteFile(filepath.Join(workdir, r, ".terraform.lock.hcl"),
			[]byte(lockfileFor(r)), 0o644); err != nil {
			return Outcome{}, "", err
		}
	}

	// --- the fixtures the tofu/git shim answers from -------------------
	fixturesJSON, err := json.Marshal(s.Fixtures)
	if err != nil {
		return Outcome{}, "", err
	}
	binDir, err := h.binDirFor(root, fixturesJSON)
	if err != nil {
		return Outcome{}, "", err
	}

	// --- the fakes ------------------------------------------------------
	seed := map[string][]byte{}
	for k, v := range s.BucketBefore {
		b, err := Body(v)
		if err != nil {
			return Outcome{}, "", fmt.Errorf("parity: %s: bucket_before[%s]: %w", s.Name, k, err)
		}
		seed[k] = b
	}
	bucket := s.Env["LEDGER_BUCKET"]
	if bucket == "" {
		bucket = "state-bucket"
	}
	ledger := newFakeLedger(bucket, seed)
	defer ledger.close()

	forge := newFakeForge(s.Fixtures, "fake-installation-token")
	defer forge.Close()

	vault := newFakeVault(vaultMount, vaultItems(s.Fixtures, now))
	defer vault.Close()

	cloudflare := newFakeCloudflare(s.Fixtures.CloudflareVerify, now)
	defer cloudflare.Close()

	proxy := newRefusingProxy()
	defer proxy.Close()

	// --- the credential mount ------------------------------------------
	secretsDir := filepath.Join(root, "secrets")
	if err := writeSecrets(secretsDir, s.Fixtures, ledger.srv.URL); err != nil {
		return Outcome{}, "", err
	}

	jwtPath := filepath.Join(root, "vault-jwt")
	if err := os.WriteFile(jwtPath, []byte("fake.jwt.token"), 0o600); err != nil {
		return Outcome{}, "", err
	}
	opTokenFile := filepath.Join(root, "op-token")
	if err := os.WriteFile(opTokenFile, []byte("fake-token-never-real"), 0o600); err != nil {
		return Outcome{}, "", err
	}

	// --- the environment -------------------------------------------------
	env := map[string]string{
		"REPO":                  "acme/platform",
		"APPROVER":              "alice",
		"LEDGER_BUCKET":         bucket,
		"LEDGER_APPLIED_PREFIX": "applied",
		"LEDGER_FAILED_PREFIX":  "failed",
		"LEDGER_HEAD_KEY":       "applied/HEAD",
		"HEARTBEAT_KEY":         "heartbeat/applier.json",
		"PLAN_DIGEST_PREFIX":    "plans",
		"WORKDIR":               workdir,
		"OP_TOKEN_FILE":         opTokenFile,
		"SECRETS_DIR":           secretsDir,
		"TF_PLUGIN_DIR":         filepath.Join(root, "providers"),

		"VAULT_ADDR":     vault.URL,
		"VAULT_ROLE":     "applier",
		"VAULT_JWT_PATH": jwtPath,
		"VAULT_MOUNT":    vaultMount,

		"GITHUB_API_BASE_URL":     forge.URL,
		"CLOUDFLARE_API_BASE_URL": cloudflare.URL,

		"PATH": binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME": root,

		// The alert is read off stdout, not off the wire; every outbound
		// https request is sent to a proxy that refuses it. See
		// newRefusingProxy.
		"HTTPS_PROXY": proxy.URL,
	}
	// HANDOFF_SOCKET: required on the DRIFT pass and forbidden on a frequent
	// one (loadHandoffConfig, design publisher-identity-design.md §3), because
	// rotation runs only under DRIFT_CHECK=1 and so only that pass has
	// anything to publish. Set here only for drift scenarios.
	//
	// ⚠️ A STUB PUBLISHER LISTENS, AND IT HAS TO. An earlier version of this
	// comment said "no publisher process is ever started for a parity run:
	// runHandoff treats a socket nobody is listening on as nothing to publish"
	// -- which was true only while truss never had anything to publish.
	//
	// Now that a successful rotation on a drift pass sets PublishValue, a dial
	// failure is CORRECTLY a pass failure (design §9), so a harness with no
	// listener made seven recorded scenarios fail against a bash reference
	// that has no concept of a publisher at all. That is not a divergence to
	// record -- in production a publisher IS listening -- it is a fixture
	// missing a component that production has, which invents a failure. The
	// fixture is the bug, so the fixture grows the component.
	//
	// It runs the REAL handoff.Serve rather than a hand-rolled socket, so the
	// wire protocol under test is the one production uses; only the verdict is
	// canned. It answers once, which is exactly what truss sends.
	//
	// ⚠️ THE VERDICT IT RETURNS IS NOT OBSERVED, AND THAT IS CORRECT. Mutating
	// this handler to always answer "skipped" changes no scenario: parity
	// compares ledger writes, the heartbeat and the alert, and a publish that
	// SUCCEEDS changes none of them. What is load-bearing is that a publisher
	// ANSWERS AT ALL -- removing the listener fails the same seven scenarios
	// that were failing before it was added. Verified both ways.
	if s.Env["DRIFT_CHECK"] == "1" {
		sock := filepath.Join(root, "handoff.sock")
		env["HANDOFF_SOCKET"] = sock
		// ⚠️ ITS OWN CANCELLABLE CONTEXT, NOT ctx DIRECTLY. handoff.Serve
		// now answers requests until its context is cancelled rather than
		// returning after exactly one (the loop's publisher is a
		// long-lived server; see handoff.go's own doc) -- so blocking on
		// `ctx` here, the whole scenario's context, would leave this
		// goroutine running past the `truss apply` subprocess exiting, and
		// the `<-done` below would hang forever. serveCancel, called after
		// the subprocess exits, is what makes Serve return.
		serveCtx, serveCancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			// The error is deliberately dropped: a scenario where truss never
			// contacts the publisher is a scenario where nothing ever connects,
			// and that must surface as the pass's own recorded outcome rather
			// than as a harness error that masks it.
			_ = handoff.Serve(serveCtx, sock, func(req handoff.Request) handoff.Response {
				if req.PublishValue {
					return handoff.Response{Value: handoff.ValueWritten, Expiries: 1}
				}
				return handoff.Response{Value: handoff.ValueSkipped}
			})
		}()
		defer func() { serveCancel(); <-done }()
	}
	for k, v := range s.Env {
		// The scenario's own recorded environment wins, so a drift run's
		// DRIFT_CHECK and its separate heartbeat key arrive here.
		env[k] = v
	}
	for _, name := range s.UnsetEnv {
		delete(env, name)
	}

	cmd := exec.CommandContext(ctx, h.Truss, "apply")
	cmd.Env = flattenEnv(env)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); !ok {
			return Outcome{}, stderr.String(), fmt.Errorf("parity: running truss apply: %w", err)
		}
		exitCode = exitErr.ExitCode()
	}

	out := Outcome{ExitCode: exitCode, BucketAfter: ledger.snapshot()}
	if text := strings.TrimRight(stdout.String(), "\n"); text != "" {
		out = out.WithAlert(text)
	}
	return out, stderr.String(), nil
}

// vaultMount is the one KV mount the sweep reads. It is not the reference
// harness's 1Password vault name by coincidence: decision 4 moved the same
// contents to a Vault mount of the same name, and the corpus records the
// contents under that name for both.
const vaultMount = "platform"

// vaultItems turns the scenario's recorded vault contents into the
// custom_metadata a Vault KV mount would carry.
//
// ⚠️ ONLY THE "platform" VAULT CROSSES. The bash sweeps two 1Password
// vaults; truss sweeps one Vault mount, because no mount exists for the
// second yet (docs/port-plan.md §4.7, "A SECOND GAP, LARGER THAN DECISION 4
// STATES"). That is a real difference in what the two report, and it is
// enumerated in divergences.go rather than papered over here -- this
// function does not invent a second mount to make the comparison come out
// even.
func vaultItems(f Fixtures, now time.Time) map[string]map[string]string {
	items := map[string]map[string]string{}

	// ⚠️ THE MOUNT IS NEVER EMPTY IN PRODUCTION, AND THIS FAKE USED TO SERVE
	// IT EMPTY WHENEVER A SCENARIO NAMED NO EXPIRY FIXTURES. The applier
	// reads its own credentials out of this mount, so if it were empty the
	// pass would have died fetching them long before the sweep ran. Serving
	// nothing made the sweep's "no items at all" refusal unreachable in
	// every parity scenario -- the same too-forgiving-fixture shape that
	// hid the expiry alarm in cmd/truss's own fake, found the same week.
	//
	// They carry no `expires`, which is also what production looks like
	// today: nothing seeds it. That is what the EXPIRY-NOT-CHECKED-CLAUSE
	// divergence records.
	for _, title := range []string{
		"gcs-ledger", "github-app", "telegram-alert",
		"cf-token-mint", "gcp-apply", "tofu-encryption",
	} {
		items[title] = nil
	}

	for title, fields := range f.Secrets[vaultMount] {
		expires, ok := fields["expires"]
		if !ok || expires == nil {
			// No `expires` recorded, which is a finding rather than a
			// skip -- a gap is a credential that will expire
			// unannounced.
			items[title] = nil
			continue
		}
		items[title] = map[string]string{"expires": ResolveDate(now, *expires)}
	}
	return items
}

// writeSecrets renders the credential mount the way the vault-secrets init
// container does: one directory per item, one file per field.
//
// Every credential exists by default, matching the reference `op` shim,
// which answers any reference it was not told about. A field the scenario
// explicitly records as nil is NOT written, which is how that shim spells
// "the item is absent". A field recorded with any other value OVERRIDES the
// default and is written verbatim -- which is how
// test_an_empty_credential_file_is_as_fatal_as_a_missing_one spells "the
// item is present but the mirror rendered it empty", a state a missing file
// cannot represent.
//
// ⚠️ THE OVERRIDE BRANCH WAS MISSING UNTIL 2026-09-08, AND THIS COMMENT USED
// TO CLAIM ONLY nil EVER MATTERED. That was true of the corpus that existed
// then. Re-recording against origin/main added a scenario whose whole point
// is an empty (not absent) app_id, and this function silently substituted
// the default "123456" for it -- the fixture was recorded correctly and the
// replay threw the override away, so the scenario "passed" without truss
// ever seeing the empty file it was written to test.
func writeSecrets(root string, f Fixtures, ledgerEndpoint string) error {
	defaults := map[string]map[string]string{
		"gcs-ledger": {
			"endpoint":          ledgerEndpoint,
			"access_key_id":     "AKIAFAKEACCESSKEYID",
			"secret_access_key": "fakesecretaccesskeyfakesecretaccesskey",
		},
		"github-app": {
			"app_id":          "123456",
			"installation_id": "7654321",
			// Generated fresh, never a fixture on disk, so leakscan never
			// sees a real-looking key -- the same convention internal/forge
			// and cmd/truss already follow.
			"private_key": "",
		},
		"telegram-alert":  {"bot_token": "fake-bot-token", "chat_id": "-100200300"},
		"cf-token-mint":   {"credential": "fake-cf-mint-token"},
		"cf-infra-admin":  {"password": "fake-cf-infra-admin-token"},
		"gcp-apply":       {"credentials": `{"type":"service_account"}`},
		"tofu-encryption": {"passphrase": "fake-passphrase"},
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	defaults["github-app"]["private_key"] = string(pem.EncodeToMemory(
		&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))

	for item, fields := range defaults {
		dir := filepath.Join(root, item)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		for field, value := range fields {
			// An explicit nil in the corpus removes the field entirely; any
			// other declared value overrides the default and is written as
			// given, including an empty string.
			if declared, ok := f.Secrets[vaultMount][item]; ok {
				if v, named := declared[field]; named {
					if v == nil {
						continue
					}
					value = *v
				}
			}
			if err := os.WriteFile(filepath.Join(dir, field), []byte(value), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// asExitError is errors.As, spelled out so this file does not import
// errors for one call and so the failure to convert is an explicit branch
// rather than a shadowed one.
func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

// lockfileFor is the committed provider set a root would really have.
// `platform/` declares only github; everything else here touches Cloudflare.
func lockfileFor(root string) string {
	const github = "provider \"registry.opentofu.org/integrations/github\" {\n  version = \"6.0.0\"\n}\n"
	const cloudflare = "provider \"registry.opentofu.org/cloudflare/cloudflare\" {\n  version = \"5.0.0\"\n}\n"
	if root == "platform" {
		return github
	}
	return cloudflare + github
}
