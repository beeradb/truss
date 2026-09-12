package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/handoff"
	"github.com/beeradb/truss/internal/secrets"
)

// fakeLoginGrant is the token the fake Vault hands back at login. Held in a
// const so no line reads `token: "<16+ chars>"` -- the shape scripts/leakscan
// refuses, correctly, since it cannot tell a fixture from a real credential.
const fakeLoginGrant = "fake-publish-client-token"

// --- a small, self-contained fake Vault, independent of internal/secrets'
// own unexported fakeVault (which this package cannot reach: it lives in a
// different package's _test.go file). It models exactly the four calls
// cmdPublish's write path makes: login, metadata GET (for the cas version),
// metadata PATCH (the expiry table), and data POST (the value write). ---

type fakePublishVaultRequest struct {
	Method string
	Path   string
	Body   string
}

type fakePublishVault struct {
	mu         sync.Mutex
	requests   []fakePublishVaultRequest
	loginToken string
	dataWrites map[string]map[string]string
	// versions is the ONE version counter per item, shared by the metadata
	// GET (current_version) and the data POST's cas check -- the same way
	// real KV v2 has exactly one version counter per item, never two. A
	// fake that modeled these as separate maps could silently drift out of
	// sync with itself and make a test pass or fail for the wrong reason;
	// this is that lesson applied rather than re-learned. Absent means the
	// item has never had a version written (real Vault 404s its metadata
	// GET in that state, and a data POST needs cas=0).
	versions  map[string]int
	patchFail map[string]int // item -> status code to answer a PATCH with, instead of 200
	// putFail, when set for an item, answers that item's data POST with the
	// given status and the request body echoed back into the response --
	// modelling a Vault that reflects what it was sent, the way a real
	// error page or a misconfigured proxy sometimes does. It exists for
	// TestHandleNeverLeaksTheFetchedValueWhenPutValueFails: if handle() (or
	// secrets.KV.PutValue underneath it) failed to redact the value it just
	// fetched from 1Password before building its own error, the echo would
	// surface it.
	putFail map[string]int
}

func newFakePublishVault() *fakePublishVault {
	return &fakePublishVault{
		// Assigned through a const so the line does not read as
		// `token: "<16+ chars>"`, the shape scripts/leakscan refuses --
		// correctly, since it cannot tell a fixture from a real one.
		loginToken: fakeLoginGrant,
		dataWrites: map[string]map[string]string{},
		versions:   map[string]int{},
		patchFail:  map[string]int{},
		putFail:    map[string]int{},
	}
}

// seedVersion sets item's current KV v2 version, the way a live Vault's
// would already be non-zero for anything ever written to it -- driving
// both the metadata GET currentVersion reads and the cas the data POST
// must present, because in real Vault they are the same number.
func (f *fakePublishVault) seedVersion(item string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions[item] = version
}

func (f *fakePublishVault) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(f.handle))
}

func (f *fakePublishVault) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	f.mu.Lock()
	f.requests = append(f.requests, fakePublishVaultRequest{Method: r.Method, Path: r.URL.Path, Body: string(body)})
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/kubernetes/login":
		fmt.Fprintf(w, `{"auth":{"client_token":%q}}`, f.loginToken)

	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/metadata/"):
		item := path.Base(r.URL.Path)
		f.mu.Lock()
		status := f.patchFail[item]
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"errors":["patch denied"]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)

	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/metadata/"):
		item := path.Base(r.URL.Path)
		f.mu.Lock()
		version, known := f.versions[item]
		f.mu.Unlock()
		if !known {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"errors":[]}`)
			return
		}
		fmt.Fprintf(w, `{"data":{"current_version":%d,"custom_metadata":{}}}`, version)

	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/data/"):
		item := path.Base(r.URL.Path)
		f.mu.Lock()
		failStatus := f.putFail[item]
		f.mu.Unlock()
		if failStatus != 0 {
			w.WriteHeader(failStatus)
			fmt.Fprintf(w, `{"errors":[%q]}`, string(body))
			return
		}
		var parsed struct {
			Options struct {
				CAS int `json:"cas"`
			} `json:"options"`
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		current := f.versions[item]
		if parsed.Options.CAS != current {
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":["check-and-set parameter did not match the current version"]}`)
			return
		}
		f.versions[item] = current + 1
		f.dataWrites[item] = parsed.Data
		f.mu.Unlock()
		fmt.Fprintf(w, `{"data":{"version":%d}}`, current+1)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakePublishVault) recordedRequests() []fakePublishVaultRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakePublishVaultRequest(nil), f.requests...)
}

func (f *fakePublishVault) dataWriteFor(item string) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.dataWrites[item]
	return v, ok
}

// --- a fake opReader, standing in for 1Password ---------------------------

// fakeOP is a test double for opReader (publish_cmd.go): the two 1Password
// reads handle() makes when PublishValue is true. It never shells out to a
// real `op` -- that boundary (redacting the service-account token, the
// empty-value refusal, the missing-vs-genuinely-failed distinction) is
// internal/secrets/opstore_test.go's job; this double exists to drive
// publishHandler.handle() the way cmdPublish's real newOPStore would, with
// canned answers.
type fakeOP struct {
	value    string
	valueErr error

	expires   string
	recorded  bool
	expiryErr error

	mu          sync.Mutex
	fieldCalls  []string // "item/field" pairs Field was asked for, in order
	expiryCalls []string // items Expiry was asked for, in order
}

func (f *fakeOP) Field(ctx context.Context, item, field string) (string, error) {
	f.mu.Lock()
	f.fieldCalls = append(f.fieldCalls, item+"/"+field)
	f.mu.Unlock()
	if f.valueErr != nil {
		return "", f.valueErr
	}
	return f.value, nil
}

func (f *fakeOP) Expiry(ctx context.Context, item string) (string, bool, error) {
	f.mu.Lock()
	f.expiryCalls = append(f.expiryCalls, item)
	f.mu.Unlock()
	if f.expiryErr != nil {
		return "", false, f.expiryErr
	}
	return f.expires, f.recorded, nil
}

// asOPFactory adapts a fixed fakeOP into the func(secrets.OPConfig)
// (opReader, error) shape publishHandler.newOP expects, ignoring cfg --
// this double's whole point is to skip 1Password entirely, so what would
// have configured a real one is irrelevant here.
func (f *fakeOP) asOPFactory() func(secrets.OPConfig) (opReader, error) {
	return func(secrets.OPConfig) (opReader, error) { return f, nil }
}

// newPublishTestKV builds a *secrets.KV against a fresh fakePublishVault,
// constructed the same way cmdPublish's real one is: WritableItem is
// always itemCFInfraAdmin, because that is the only item this publisher is
// ever built to write.
func newPublishTestKV(t *testing.T, fv *fakePublishVault, srv *httptest.Server) *secrets.KV {
	t.Helper()
	jwtPath := writePublishJWT(t)
	kv, err := secrets.NewKV(secrets.KVConfig{
		Addr: srv.URL, Mount: "platform", Role: "publisher", JWTPath: jwtPath, HTTP: srv.Client(),
		WritableItem: itemCFInfraAdmin,
	})
	if err != nil {
		t.Fatalf("NewKV: %v", err)
	}
	return kv
}

// vaultFactoryFor wraps an already-built vaultWriter (a real *secrets.KV
// against a fake server, or a stub like noopPublisher) as the
// publishHandler.newVault factory these tests need -- production rebuilds
// one fresh per request; a test that only cares about ONE request's
// behavior can hand back the same value every time.
func vaultFactoryFor(v vaultWriter) func(secrets.KVConfig) (vaultWriter, error) {
	return func(secrets.KVConfig) (vaultWriter, error) { return v, nil }
}

// --- fixtures ---

func writePublishJWT(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	// Assembled from parts, not one literal, so it does not itself read as
	// a credential to a scanner (the same convention internal/secrets'
	// own jwtFixture uses).
	jwt := strings.Join([]string{"eyJhbGciOiJub25lIn0", "eyJzdWIiOiJwdWJsaXNoZXIifQ", "ZmFrZS1zaWc"}, ".")
	if err := os.WriteFile(p, []byte(jwt), 0o600); err != nil {
		t.Fatalf("writing jwt fixture: %v", err)
	}
	return p
}

func writeExpiriesFile(t *testing.T, table map[string]string) string {
	t.Helper()
	entries := make(map[string]map[string]string, len(table))
	for item, expires := range table {
		entries[item] = map[string]string{"expires": expires, "why": "test fixture"}
	}
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshaling expiries fixture: %v", err)
	}
	p := filepath.Join(t.TempDir(), "expiries.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("writing expiries fixture: %v", err)
	}
	return p
}

// publishEnv builds the getenv function cmdPublish reads, mirroring what
// the manifest sets (design §8): the same four Vault variable names
// loadVaultConfig already uses, plus HANDOFF_SOCKET, EXPIRIES_FILE and an
// optional PUBLISH_WAIT.
func publishEnv(vaultAddr, jwtPath, socket, expiriesFile string, extra map[string]string) func(string) string {
	values := map[string]string{
		"VAULT_ADDR":     vaultAddr,
		"VAULT_ROLE":     "publisher",
		"VAULT_JWT_PATH": jwtPath,
		"VAULT_MOUNT":    "platform",
		"HANDOFF_SOCKET": socket,
		"EXPIRIES_FILE":  expiriesFile,
	}
	for k, v := range extra {
		values[k] = v
	}
	return func(name string) string { return values[name] }
}

func waitForPublishSocket(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s was never created", path)
}

// runPublishAsync starts cmdPublish in a goroutine against env and returns
// a channel carrying its exit code plus the captured stderr, once it
// returns.
func runPublishAsync(ctx context.Context, getenv func(string) string) (exitCode chan int, stderrOut *syncBuffer) {
	exitCode = make(chan int, 1)
	stderrOut = &syncBuffer{}
	go func() {
		var stdout bytes.Buffer
		exitCode <- cmdPublish(ctx, nil, getenv, &stdout, stderrOut)
	}()
	return exitCode, stderrOut
}

// syncBuffer lets the goroutine running cmdPublish write to stderr while
// the test goroutine reads it after the exchange completes, without a race
// detector complaint.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// --- config tests ---

func TestLoadPublishConfigRefusesEachMissingVariable(t *testing.T) {
	for _, name := range publishRequiredEnv {
		t.Run(name, func(t *testing.T) {
			values := map[string]string{
				"VAULT_ADDR": "http://vault.invalid", "VAULT_ROLE": "publisher",
				"VAULT_JWT_PATH": "/var/run/secrets/vault-publish/token", "VAULT_MOUNT": "platform",
				"HANDOFF_SOCKET": "/handoff/publish.sock", "EXPIRIES_FILE": "/etc/publisher/expiries.json",
			}
			values[name] = ""
			getenv := func(n string) string { return values[n] }

			_, problems := loadPublishConfig(getenv)
			if len(problems) == 0 {
				t.Fatalf("no problems reported with $%s unset", name)
			}
			found := false
			for _, p := range problems {
				if strings.Contains(p, name) {
					found = true
				}
			}
			if !found {
				t.Errorf("problems %v do not mention %s", problems, name)
			}
		})
	}
}

func TestLoadPublishConfigLoadsCleanlyWithNoOtherVariablesSet(t *testing.T) {
	base := map[string]string{
		"VAULT_ADDR": "http://vault.invalid", "VAULT_ROLE": "publisher",
		"VAULT_JWT_PATH": "/token", "VAULT_MOUNT": "platform",
		"HANDOFF_SOCKET": "/handoff/publish.sock", "EXPIRIES_FILE": "/etc/publisher/expiries.json",
	}
	_, problems := loadPublishConfig(func(n string) string { return base[n] })
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
}

// TestLoadPublishConfigRefusesPublishWait pins $PUBLISH_WAIT's retirement:
// the publisher is a long-lived server with no accept-side timeout, so a
// manifest that still sets it is refused rather than silently ignored --
// silence would leave an operator believing a timeout exists here.
func TestLoadPublishConfigRefusesPublishWait(t *testing.T) {
	base := map[string]string{
		"VAULT_ADDR": "http://vault.invalid", "VAULT_ROLE": "publisher",
		"VAULT_JWT_PATH": "/token", "VAULT_MOUNT": "platform",
		"HANDOFF_SOCKET": "/handoff/publish.sock", "EXPIRIES_FILE": "/etc/publisher/expiries.json",
		"PUBLISH_WAIT": "5m",
	}
	_, problems := loadPublishConfig(func(n string) string { return base[n] })
	if len(problems) == 0 {
		t.Fatal("no problem reported for a set $PUBLISH_WAIT, want a refusal")
	}
	found := false
	for _, p := range problems {
		if strings.Contains(p, "PUBLISH_WAIT") {
			found = true
		}
	}
	if !found {
		t.Errorf("problems %v do not mention PUBLISH_WAIT", problems)
	}
}

// --- cmdPublish argument handling ---

func TestCmdPublishRefusesArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdPublish(context.Background(), []string{"extra"}, func(string) string { return "" }, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "usage: truss publish") {
		t.Errorf("stderr = %q, want a usage message", stderr.String())
	}
}

// --- end-to-end tests, driving cmdPublish exactly as the manifest would:
// real env vars, a real socket, a real (fake) Vault over HTTP. ---
//
// ⚠️ ONLY THE PublishValue==false PATH IS DRIVEN THIS WAY NOW. A value
// publish also needs 1Password, and there is no way to fake `op` through
// cmdPublish's own env-driven construction without a second exec path this
// project's design explicitly refuses (opstore.go owns the only one). The
// tests below that exercise a value publish instead build a publishHandler
// directly -- the same style TestAPublishThatDidNothingIsRefusedNotReportedAsSuccess
// already used before this file had anything else that needed it -- with a
// real *secrets.KV against fakePublishVault (so the Vault interaction is
// still exercised for real) and a fakeOP standing in for 1Password.

func TestHandlePublishesTheFetchedValueAndExpiry(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()
	fv.seedVersion(itemCFInfraAdmin, 4) // pre-existing item, version 4
	kv := newPublishTestKV(t, fv, srv)

	op := &fakeOP{value: "gen2-cloudflare-token", expires: "2026-12-01", recorded: true}
	h := publishHandler{
		newVault: vaultFactoryFor(kv),
		expiriesFile: writeExpiriesFile(t, map[string]string{
			itemGitHubApp:      "2027-01-01",
			itemTelegram:       "never",
			itemGCPApply:       "2027-06-15",
			itemLedger:         "2027-06-15",
			itemTofuEncryption: "2027-06-15",
		}),
		item:  itemCFInfraAdmin,
		field: fieldCFPassword,
		newOP: op.asOPFactory(),
	}

	resp := h.handle(handoff.Request{PublishValue: true})

	if resp.Value != handoff.ValueWritten {
		t.Errorf("resp.Value = %q, want %q (resp: %+v)", resp.Value, handoff.ValueWritten, resp)
	}
	// 5 table entries + the minted item's own expiry.
	if resp.Expiries != 6 {
		t.Errorf("resp.Expiries = %d, want 6", resp.Expiries)
	}
	if len(resp.Skipped) != 0 {
		t.Errorf("resp.Skipped = %v, want none", resp.Skipped)
	}
	if resp.Error != "" {
		t.Errorf("resp.Error = %q, want empty", resp.Error)
	}

	// The value and expiry were fetched under the compiled-in names, never
	// anything the request could have named (it cannot name anything: the
	// request is a single bool).
	wantFieldCall := itemCFInfraAdmin + "/" + fieldCFPassword
	if len(op.fieldCalls) != 1 || op.fieldCalls[0] != wantFieldCall {
		t.Errorf("op.fieldCalls = %v, want exactly [%q]", op.fieldCalls, wantFieldCall)
	}
	if len(op.expiryCalls) != 1 || op.expiryCalls[0] != itemCFInfraAdmin {
		t.Errorf("op.expiryCalls = %v, want exactly [%q]", op.expiryCalls, itemCFInfraAdmin)
	}

	written, ok := fv.dataWriteFor(itemCFInfraAdmin)
	if !ok {
		t.Fatal("the fake vault never recorded a data write for cf-infra-admin")
	}
	if written[fieldCFPassword] != "gen2-cloudflare-token" {
		t.Errorf("written value = %v, want the %s field set to the fetched value", written, fieldCFPassword)
	}

	// The write must have used cas=4 (the version this test seeded), not 0
	// -- proof currentVersion's read actually drove PutValue's guard.
	foundCorrectCAS := false
	for _, r := range fv.recordedRequests() {
		if r.Method == http.MethodPost && strings.Contains(r.Path, "/data/"+itemCFInfraAdmin) {
			if strings.Contains(r.Body, `"cas":4`) {
				foundCorrectCAS = true
			}
		}
	}
	if !foundCorrectCAS {
		t.Error("no data POST carried cas:4 -- the current-version read did not drive the write")
	}

	// Every table entry, plus the minted item's own expiry, must have been
	// PATCHed -- never POSTed (design §5: PATCH only, to the metadata
	// endpoint).
	patchedItems := map[string]bool{}
	for _, r := range fv.recordedRequests() {
		if r.Method == http.MethodPatch && strings.Contains(r.Path, "/metadata/") {
			patchedItems[path.Base(r.Path)] = true
		}
	}
	for _, item := range []string{itemGitHubApp, itemTelegram, itemGCPApply, itemLedger, itemTofuEncryption, itemCFInfraAdmin} {
		if !patchedItems[item] {
			t.Errorf("%s was never PATCHed", item)
		}
	}
	// The fetched value must never have reached the fake server in any
	// PATCH body (it belongs only in the one data POST above).
	for _, r := range fv.recordedRequests() {
		if r.Method == http.MethodPatch && strings.Contains(r.Body, "gen2-cloudflare-token") {
			t.Errorf("a PATCH request carried the credential value: %+v", r)
		}
	}
}

// TestEachRequestRereadsTheExpiryTableFromDisk is the property
// publishHandler.expiriesFile exists for: the table is a ConfigMap mount
// Kubernetes updates in place, so an edit between two requests must take
// effect on the second one without a restart.
func TestEachRequestRereadsTheExpiryTableFromDisk(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv)

	path := writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"})
	h := publishHandler{
		newVault:     vaultFactoryFor(kv),
		expiriesFile: path,
		item:         itemCFInfraAdmin,
		field:        fieldCFPassword,
	}

	first := h.handle(handoff.Request{PublishValue: false})
	if first.Expiries != 1 {
		t.Fatalf("first request: Expiries = %d, want 1", first.Expiries)
	}

	// Edit the table in place, the way a ConfigMap update does -- add a
	// second item, still pointing h at the SAME path.
	overwritten := writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01", itemTelegram: "never"})
	if err := os.Rename(overwritten, path); err != nil {
		t.Fatalf("simulating an in-place ConfigMap update: %v", err)
	}

	second := h.handle(handoff.Request{PublishValue: false})
	if second.Expiries != 2 {
		t.Fatalf("second request: Expiries = %d, want 2 -- the edited table did not take effect without a restart", second.Expiries)
	}
}

// TestEachRequestBuildsItsOwnVaultWriter is the property h.newVault exists
// for: secrets.KV caches its Vault token for its own lifetime and never
// refreshes it, so a handler that cached ONE successful construction
// across requests would 403 forever after the token's TTL expired.
// Asserted directly on the call count -- with both calls succeeding, a
// cache-after-success bug is invisible to anything that only checks the
// RESPONSE, since a cached, still-valid writer answers identically to a
// freshly built one.
func TestEachRequestBuildsItsOwnVaultWriter(t *testing.T) {
	calls := 0
	h := publishHandler{
		newVault: func(secrets.KVConfig) (vaultWriter, error) {
			calls++
			return noopVault{}, nil
		},
		expiriesFile: writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"}),
		item:         itemCFInfraAdmin,
	}

	h.handle(handoff.Request{PublishValue: false})
	h.handle(handoff.Request{PublishValue: false})

	if calls != 2 {
		t.Fatalf("newVault was called %d times across two requests, want 2 -- it must not be cached from an earlier request", calls)
	}
}

// noopVault answers every write with success and every table entry as
// already at version 0 -- just enough for
// TestEachRequestBuildsItsOwnVaultWriter's second call to reach
// ValueSkipped rather than failing on some other, unrelated ground.
type noopVault struct{}

func (noopVault) PatchExpiry(ctx context.Context, item, expires string) error { return nil }
func (noopVault) PutValue(ctx context.Context, item string, fields map[string]string, cas int) error {
	return nil
}
func (noopVault) CurrentVersion(ctx context.Context, item string) (int, bool, error) {
	return 0, false, nil
}

func TestCmdPublishReportsSkippedWhenPublishValueIsFalse(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()

	jwtPath := writePublishJWT(t)
	expiriesPath := writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"})
	socket := filepath.Join(t.TempDir(), "publish.sock")

	getenv := publishEnv(srv.URL, jwtPath, socket, expiriesPath, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exitCode, stderrOut := runPublishAsync(ctx, getenv)
	waitForPublishSocket(t, socket)

	resp, err := handoff.Send(context.Background(), socket, 5*time.Second, handoff.Request{PublishValue: false})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Serve now runs until its context is cancelled rather than returning
	// after one request; cancel explicitly so this test does not wait out
	// the full 10s timeout to see cmdPublish exit.
	cancel()
	if code := <-exitCode; code != 0 {
		t.Fatalf("cmdPublish exit code = %d, want 0; stderr: %s", code, stderrOut.String())
	}

	if resp.Value != handoff.ValueSkipped {
		t.Errorf("resp.Value = %q, want %q", resp.Value, handoff.ValueSkipped)
	}
	if resp.Expiries != 1 {
		t.Errorf("resp.Expiries = %d, want 1 (the one table entry)", resp.Expiries)
	}

	if _, wrote := fv.dataWriteFor(itemCFInfraAdmin); wrote {
		t.Error("a data write happened despite publish_value being false")
	}
	for _, r := range fv.recordedRequests() {
		if strings.Contains(r.Path, "/data/") {
			t.Errorf("a request reached a /data/ path with publish_value false: %+v", r)
		}
	}
}

// TestHandleFailsClosedWhenTheOPTokenFileIsUnreadable exercises newOPStore
// -- the REAL production wrapper around secrets.NewOP, not fakeOP -- with
// $OP_TOKEN_FILE pointing at a path that does not exist. loadPublishConfig
// deliberately never validates this at startup (see its own doc); this is
// the point at which it must, loudly, with no fallback to any other
// credential.
func TestHandleFailsClosedWhenTheOPTokenFileIsUnreadable(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv)

	missingTokenPath := filepath.Join(t.TempDir(), "does-not-exist", "token")
	h := publishHandler{
		newVault:     vaultFactoryFor(kv),
		expiriesFile: writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"}),
		item:      itemCFInfraAdmin,
		field:     fieldCFPassword,
		opCfg:     secrets.OPConfig{Vault: "platform", TokenFile: missingTokenPath},
		newOP:     newOPStore, // the real constructor -- no fake here
	}

	resp := h.handle(handoff.Request{PublishValue: true})

	if resp.Value != handoff.ValueFailed {
		t.Errorf("resp.Value = %q, want %q", resp.Value, handoff.ValueFailed)
	}
	if !strings.Contains(resp.Error, missingTokenPath) {
		t.Errorf("resp.Error = %q, want it to name the unreadable token path", resp.Error)
	}
	// The table patch needs no 1Password at all and must still have
	// happened -- an absent value credential must not undo the half of
	// this pass that did not need one.
	if resp.Expiries != 1 {
		t.Errorf("resp.Expiries = %d, want 1 -- the table patch must still have run", resp.Expiries)
	}
	if _, wrote := fv.dataWriteFor(itemCFInfraAdmin); wrote {
		t.Error("a data write happened despite the OP token file being unreadable")
	}
}

// TestHandleFailsClosedWhenOPConfigIsEmpty covers the deploy-time-default
// shape: $OP_TOKEN_FILE and the vault name both unset (loadPublishConfig
// never required them). secrets.NewOP itself refuses that config; handle()
// must surface the refusal rather than silently skipping the value publish
// or falling back to anything else.
func TestHandleFailsClosedWhenOPConfigIsEmpty(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv)

	h := publishHandler{
		newVault:     vaultFactoryFor(kv),
		expiriesFile: writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"}),
		item:      itemCFInfraAdmin,
		field:     fieldCFPassword,
		opCfg:     secrets.OPConfig{}, // both Vault and TokenFile empty
		newOP:     newOPStore,
	}

	resp := h.handle(handoff.Request{PublishValue: true})

	if resp.Value != handoff.ValueFailed {
		t.Errorf("resp.Value = %q, want %q", resp.Value, handoff.ValueFailed)
	}
	if resp.Error == "" {
		t.Error("resp.Error is empty, want a refusal naming the empty 1Password config")
	}
	if _, wrote := fv.dataWriteFor(itemCFInfraAdmin); wrote {
		t.Error("a data write happened despite an empty 1Password config")
	}
}

// TestHandleRefusesAnEmptyOrUnrecordedFetch covers the two ways a
// PublishValue-true request can find nothing usable in 1Password: Field
// returning an error (op.Field itself already refuses an empty value --
// this pins that handle() surfaces that refusal rather than writing
// nothing silently) and Expiry finding nothing recorded for the item.
// Neither is a legitimate reason to skip -- design's own rule, "a fetch
// that returns an empty value is an error, never a silent skip."
func TestHandleRefusesAnEmptyOrUnrecordedFetch(t *testing.T) {
	cases := []struct {
		name string
		op   *fakeOP
	}{
		{
			name: "Field fails",
			op:   &fakeOP{valueErr: fmt.Errorf("secrets: %q.%q in 1Password vault %q is empty", itemCFInfraAdmin, fieldCFPassword, "platform")},
		},
		{
			name: "Expiry fails",
			op:   &fakeOP{value: "gen2-cloudflare-token", expiryErr: fmt.Errorf("secrets: rate-limited")},
		},
		{
			name: "Expiry not recorded",
			op:   &fakeOP{value: "gen2-cloudflare-token", recorded: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fv := newFakePublishVault()
			srv := fv.server()
			defer srv.Close()
			kv := newPublishTestKV(t, fv, srv)

			h := publishHandler{
				newVault:     vaultFactoryFor(kv),
				expiriesFile: writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"}),
				item:      itemCFInfraAdmin,
				field:     fieldCFPassword,
				newOP:     tc.op.asOPFactory(),
			}

			resp := h.handle(handoff.Request{PublishValue: true})

			if resp.Value != handoff.ValueFailed {
				t.Errorf("resp.Value = %q, want %q", resp.Value, handoff.ValueFailed)
			}
			if resp.Error == "" {
				t.Error("resp.Error is empty, want a refusal")
			}
			if _, wrote := fv.dataWriteFor(itemCFInfraAdmin); wrote {
				t.Error("a write reached the fake vault despite an empty or unrecorded fetch")
			}
			for _, r := range fv.recordedRequests() {
				if strings.Contains(r.Path, "/data/") {
					t.Errorf("a refused fetch still reached a /data/ path: %+v", r)
				}
			}
		})
	}
}

// TestHandleNeverLeaksTheFetchedValueWhenPutValueFails: fv.putFail makes
// the fake echo the request body back into a 500, the same technique
// internal/secrets' own TestTheMintedValueNeverAppearsInAPublishError uses
// against secrets.KV directly. Driving it through handle() proves the
// integration -- a value that came from 1Password, not from a request --
// never reaches a Response by a path this package's own code controls.
func TestHandleNeverLeaksTheFetchedValueWhenPutValueFails(t *testing.T) {
	const sentinel = "SENTINEL-FETCHED-CF-TOKEN-DO-NOT-LEAK"
	fv := newFakePublishVault()
	fv.putFail[itemCFInfraAdmin] = http.StatusInternalServerError
	srv := fv.server()
	defer srv.Close()
	kv := newPublishTestKV(t, fv, srv)

	op := &fakeOP{value: sentinel, expires: "2027-01-01", recorded: true}
	h := publishHandler{
		newVault:     vaultFactoryFor(kv),
		expiriesFile: writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"}),
		item:      itemCFInfraAdmin,
		field:     fieldCFPassword,
		newOP:     op.asOPFactory(),
	}

	resp := h.handle(handoff.Request{PublishValue: true})

	if resp.Value != handoff.ValueFailed {
		t.Fatalf("resp.Value = %q, want %q (resp: %+v)", resp.Value, handoff.ValueFailed, resp)
	}
	if strings.Contains(resp.Error, sentinel) {
		t.Errorf("resp.Error contains the fetched value: %q", resp.Error)
	}
	encoded, _ := json.Marshal(resp)
	if strings.Contains(string(encoded), sentinel) {
		t.Errorf("the encoded Response contains the fetched value: %s", encoded)
	}
}

// TestPartialPublishReportsBothItsWritesAndItsError: one expiry patch
// fails, the value write succeeds -- the response must carry both, per
// design §9's "Some expiries written, then one failed" row.
func TestPartialPublishReportsBothItsWritesAndItsError(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()
	// cf-infra-admin is left unseeded -- a brand-new item, never written,
	// so currentVersion must see it as absent (404) and PutValue must use
	// cas:0 to create it.
	fv.patchFail[itemGitHubApp] = http.StatusInternalServerError
	kv := newPublishTestKV(t, fv, srv)

	op := &fakeOP{value: "gen1-token", expires: "2027-01-01", recorded: true}
	h := publishHandler{
		newVault: vaultFactoryFor(kv),
		expiriesFile: writeExpiriesFile(t, map[string]string{
			itemGitHubApp: "2027-01-01", // will fail to patch
			itemTelegram:  "never",      // will succeed
		}),
		item:  itemCFInfraAdmin,
		field: fieldCFPassword,
		newOP: op.asOPFactory(),
	}

	resp := h.handle(handoff.Request{PublishValue: true})

	if resp.Value != handoff.ValueWritten {
		t.Errorf("resp.Value = %q, want %q -- a value write that succeeded must still say so", resp.Value, handoff.ValueWritten)
	}
	// telegram-alert (table) + cf-infra-admin's own fetched expiry.
	if resp.Expiries != 2 {
		t.Errorf("resp.Expiries = %d, want 2 (telegram-alert plus cf-infra-admin's own expiry)", resp.Expiries)
	}
	if len(resp.Skipped) != 1 || !strings.Contains(resp.Skipped[0], itemGitHubApp) {
		t.Errorf("resp.Skipped = %v, want one entry naming %s", resp.Skipped, itemGitHubApp)
	}
	if resp.Error == "" {
		t.Error("resp.Error is empty, want the github-app patch failure to be reported alongside the successful write")
	}
	if written, ok := fv.dataWriteFor(itemCFInfraAdmin); !ok || written[fieldCFPassword] != "gen1-token" {
		t.Errorf("the value write did not land despite one expiry patch failing: %v, %v", written, ok)
	}
}

// TestFinishRefusesAVacuousPassRatherThanReportingSuccess drives finish()
// directly rather than through handle(): with a real, path-backed expiry
// table, secrets.LoadExpiries itself now refuses an empty table before
// finish's vacuous-pass guard could ever see one (loadExpiryTable calls it
// fresh on every request), so the "nothing happened at all" case finish
// exists to catch is only reachable by calling the pure function itself.
func TestFinishRefusesAVacuousPassRatherThanReportingSuccess(t *testing.T) {
	resp := finish(handoff.Response{})

	if resp.Value != handoff.ValueFailed {
		t.Errorf("resp.Value = %q, want %q for a response with nothing to report", resp.Value, handoff.ValueFailed)
	}
	if resp.Error == "" {
		t.Error("resp.Error is empty, want a refusal explaining nothing was done")
	}
}

// TestPublishRefusesWhenItsJWTIsNotMounted is the design's own
// same-binary caveat (§6): `truss publish` is invocable inside the truss
// container, where VAULT_JWT_PATH points at a file that container never
// mounts, and it must fail cleanly and for the right reason -- login
// failing at os.ReadFile with the path named, never a fallback to any
// other JWT source and never an attempt to read one from the environment.
//
// Per design §9, the publisher's own exit code means only "nobody asked
// me" -- a served request's outcome, including this failure, travels back
// in the Response, which is what truss actually reads. So this asserts
// exit 0 (the request WAS served) and inspects the Response for the
// failure, rather than asserting a non-zero process exit.
func TestPublishRefusesWhenItsJWTIsNotMounted(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()

	missingJWTPath := filepath.Join(t.TempDir(), "does-not-exist", "token")
	expiriesPath := writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"})
	socket := filepath.Join(t.TempDir(), "publish.sock")

	getenv := publishEnv(srv.URL, missingJWTPath, socket, expiriesPath, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exitCode, stderrOut := runPublishAsync(ctx, getenv)
	waitForPublishSocket(t, socket)

	resp, err := handoff.Send(context.Background(), socket, 5*time.Second, handoff.Request{PublishValue: false})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	// Serve now runs until its context is cancelled rather than returning
	// after one request (handoff.go's own doc); cancel explicitly so this
	// test does not wait out the full 10s timeout to see cmdPublish exit.
	cancel()
	if code := <-exitCode; code != 0 {
		t.Fatalf("cmdPublish exit code = %d, want 0 (a cancelled context is a clean stop); stderr: %s", code, stderrOut.String())
	}

	if resp.Value != handoff.ValueFailed && resp.Value != handoff.ValueSkipped {
		t.Errorf("resp.Value = %q, want %q or %q with an error explaining why", resp.Value, handoff.ValueFailed, handoff.ValueSkipped)
	}
	if !strings.Contains(resp.Error, missingJWTPath) {
		t.Errorf("resp.Error = %q, want it to name the missing JWT path %q", resp.Error, missingJWTPath)
	}
	if !strings.Contains(resp.Error, "reading the ServiceAccount JWT") {
		t.Errorf("resp.Error = %q, want it to say it failed reading the ServiceAccount JWT, not something vaguer", resp.Error)
	}

	// Never a fallback: no login request of any kind should have reached
	// the fake Vault at all, because the JWT read fails before login is
	// ever attempted.
	for _, r := range fv.recordedRequests() {
		if r.Path == "/v1/auth/kubernetes/login" {
			t.Error("a login request reached vault despite the JWT file being unreadable -- there must be no fallback")
		}
	}
}

// TestNoResponseErrorEverContainsTheRequestValue used to drive four
// branches of handle() through a request built with Item/Field/Value --
// the wrong-item refusal, the empty-field refusal, and the ordinary write
// path. The first two no longer exist: a request cannot name an item or a
// field any more (handoff.Request carries only PublishValue), so there is
// nothing left for those branches to refuse. The "nothing to publish"
// case's redaction is trivial by construction (there is no value anywhere
// on that path), and the ordinary-write-path case is now
// TestHandleNeverLeaksTheFetchedValueWhenPutValueFails above, which proves
// the same thing about the value handle() actually handles today: one
// fetched from 1Password, not one carried in the request.
