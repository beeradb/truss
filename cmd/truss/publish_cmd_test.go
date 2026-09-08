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

func TestLoadPublishConfigDefaultsPublishWaitAndAcceptsAnOverride(t *testing.T) {
	base := map[string]string{
		"VAULT_ADDR": "http://vault.invalid", "VAULT_ROLE": "publisher",
		"VAULT_JWT_PATH": "/token", "VAULT_MOUNT": "platform",
		"HANDOFF_SOCKET": "/handoff/publish.sock", "EXPIRIES_FILE": "/etc/publisher/expiries.json",
	}
	cfg, problems := loadPublishConfig(func(n string) string { return base[n] })
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if cfg.wait != defaultPublishWait {
		t.Errorf("wait = %s, want the default %s", cfg.wait, defaultPublishWait)
	}

	withOverride := map[string]string{}
	for k, v := range base {
		withOverride[k] = v
	}
	withOverride["PUBLISH_WAIT"] = "5m"
	cfg, problems = loadPublishConfig(func(n string) string { return withOverride[n] })
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if cfg.wait != 5*time.Minute {
		t.Errorf("wait = %s, want 5m", cfg.wait)
	}
}

func TestLoadPublishConfigRefusesAnInvalidPublishWaitDuration(t *testing.T) {
	base := map[string]string{
		"VAULT_ADDR": "http://vault.invalid", "VAULT_ROLE": "publisher",
		"VAULT_JWT_PATH": "/token", "VAULT_MOUNT": "platform",
		"HANDOFF_SOCKET": "/handoff/publish.sock", "EXPIRIES_FILE": "/etc/publisher/expiries.json",
		"PUBLISH_WAIT": "not-a-duration",
	}
	_, problems := loadPublishConfig(func(n string) string { return base[n] })
	if len(problems) == 0 {
		t.Fatal("no problem reported for an invalid PUBLISH_WAIT")
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

func TestCmdPublishEndToEndPatchesExpiriesAndWritesTheValue(t *testing.T) {
	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()
	fv.seedVersion(itemCFInfraAdmin, 4) // pre-existing item, version 4

	jwtPath := writePublishJWT(t)
	expiriesPath := writeExpiriesFile(t, map[string]string{
		itemGitHubApp:      "2027-01-01",
		itemTelegram:       "never",
		itemGCPApply:       "2027-06-15",
		itemLedger:         "2027-06-15",
		itemTofuEncryption: "2027-06-15",
	})
	socket := filepath.Join(t.TempDir(), "publish.sock")

	getenv := publishEnv(srv.URL, jwtPath, socket, expiriesPath, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exitCode, stderrOut := runPublishAsync(ctx, getenv)
	waitForPublishSocket(t, socket)

	req := handoff.Request{
		PublishValue: true,
		Item:         itemCFInfraAdmin,
		Field:        fieldCFPassword,
		Value:        "gen2-cloudflare-token",
		Expires:      "2026-12-01",
	}
	resp, err := handoff.Send(context.Background(), socket, 5*time.Second, req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if code := <-exitCode; code != 0 {
		t.Fatalf("cmdPublish exit code = %d, want 0; stderr: %s", code, stderrOut.String())
	}

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

	written, ok := fv.dataWriteFor(itemCFInfraAdmin)
	if !ok {
		t.Fatal("the fake vault never recorded a data write for cf-infra-admin")
	}
	if written[fieldCFPassword] != "gen2-cloudflare-token" {
		t.Errorf("written value = %v, want the %s field set to the minted value", written, fieldCFPassword)
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
	// The sentinel value must never have reached the fake server in any
	// PATCH body (it belongs only in the one data POST above).
	for _, r := range fv.recordedRequests() {
		if r.Method == http.MethodPatch && strings.Contains(r.Body, "gen2-cloudflare-token") {
			t.Errorf("a PATCH request carried the credential value: %+v", r)
		}
	}
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

func TestCmdPublishRefusesAnItemOtherThanTheWritableOne(t *testing.T) {
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

	resp, err := handoff.Send(context.Background(), socket, 5*time.Second, handoff.Request{
		PublishValue: true, Item: "some-other-item", Field: "password", Value: "x",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if code := <-exitCode; code != 0 {
		t.Fatalf("cmdPublish exit code = %d, want 0; stderr: %s", code, stderrOut.String())
	}

	if resp.Value != handoff.ValueFailed {
		t.Errorf("resp.Value = %q, want %q", resp.Value, handoff.ValueFailed)
	}
	if resp.Error == "" {
		t.Error("resp.Error is empty, want a refusal naming the item")
	}
	if _, wrote := fv.dataWriteFor("some-other-item"); wrote {
		t.Error("a write for the refused item reached the fake vault")
	}
	for _, r := range fv.recordedRequests() {
		if strings.Contains(r.Path, "/data/") {
			t.Errorf("a refused item still reached a /data/ path: %+v", r)
		}
	}
}

// TestCmdPublishRefusesAnEmptyFieldOrValue: secrets.KV.PutValue refuses an
// empty or whitespace VALUE on its own, but does not validate the FIELD
// name -- a request with an empty Field and a non-empty Value would
// otherwise reach Vault as a write to a field literally named "", which
// nothing downstream expects. This is handle()'s own guard, and it must
// refuse before ever calling currentVersion or PutValue.
func TestCmdPublishRefusesAnEmptyFieldOrValue(t *testing.T) {
	cases := map[string]handoff.Request{
		"empty field": {PublishValue: true, Item: itemCFInfraAdmin, Field: "", Value: "some-value"},
		"empty value": {PublishValue: true, Item: itemCFInfraAdmin, Field: fieldCFPassword, Value: ""},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
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

			resp, err := handoff.Send(context.Background(), socket, 5*time.Second, req)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if code := <-exitCode; code != 0 {
				t.Fatalf("cmdPublish exit code = %d, want 0; stderr: %s", code, stderrOut.String())
			}

			if resp.Value != handoff.ValueFailed {
				t.Errorf("resp.Value = %q, want %q", resp.Value, handoff.ValueFailed)
			}
			if resp.Error == "" {
				t.Error("resp.Error is empty, want a refusal")
			}
			if _, wrote := fv.dataWriteFor(itemCFInfraAdmin); wrote {
				t.Error("a write reached the fake vault despite an empty field or value")
			}
			for _, r := range fv.recordedRequests() {
				if strings.Contains(r.Path, "/data/") || (strings.Contains(r.Path, "/metadata/"+itemCFInfraAdmin) && r.Method == http.MethodGet) {
					t.Errorf("the refused request still reached Vault: %+v", r)
				}
			}
		})
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

	jwtPath := writePublishJWT(t)
	expiriesPath := writeExpiriesFile(t, map[string]string{
		itemGitHubApp: "2027-01-01", // will fail to patch
		itemTelegram:  "never",      // will succeed
	})
	socket := filepath.Join(t.TempDir(), "publish.sock")

	getenv := publishEnv(srv.URL, jwtPath, socket, expiriesPath, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exitCode, stderrOut := runPublishAsync(ctx, getenv)
	waitForPublishSocket(t, socket)

	resp, err := handoff.Send(context.Background(), socket, 5*time.Second, handoff.Request{
		PublishValue: true, Item: itemCFInfraAdmin, Field: fieldCFPassword, Value: "gen1-token",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if code := <-exitCode; code != 0 {
		t.Fatalf("cmdPublish exit code = %d, want 0; stderr: %s", code, stderrOut.String())
	}

	if resp.Value != handoff.ValueWritten {
		t.Errorf("resp.Value = %q, want %q -- a value write that succeeded must still say so", resp.Value, handoff.ValueWritten)
	}
	if resp.Expiries != 1 {
		t.Errorf("resp.Expiries = %d, want 1 (only telegram-alert succeeded)", resp.Expiries)
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

// TestAPublishThatDidNothingIsRefusedNotReportedAsSuccess drives
// publishHandler directly (bypassing secrets.LoadExpiries, which already
// refuses an empty table on its own) to construct the otherwise
// unreachable "nothing happened at all" case and confirm the vacuous-pass
// guard converts it to a failure rather than a silent, contentless success.
func TestAPublishThatDidNothingIsRefusedNotReportedAsSuccess(t *testing.T) {
	h := publishHandler{
		publisher: noopPublisher{},
		versions:  noopVersionReader{},
		table:     secrets.Expiries{}, // empty: no LoadExpiries in this path to refuse it
		item:      itemCFInfraAdmin,
	}
	resp := h.handle(handoff.Request{PublishValue: false})

	if resp.Value != handoff.ValueFailed {
		t.Errorf("resp.Value = %q, want %q for a pass with nothing to do", resp.Value, handoff.ValueFailed)
	}
	if resp.Error == "" {
		t.Error("resp.Error is empty, want a refusal explaining nothing was done")
	}
	if resp.Expiries != 0 || len(resp.Skipped) != 0 {
		t.Errorf("resp = %+v, want Expiries=0 and no Skipped entries for this scenario", resp)
	}
}

// noopPublisher and noopVersionReader back
// TestAPublishThatDidNothingIsRefusedNotReportedAsSuccess: that test's
// point is the empty-table branch, which never calls either.
type noopPublisher struct{}

func (noopPublisher) PatchExpiry(ctx context.Context, item, expires string) error {
	panic("PatchExpiry should not be called with an empty table and no requested expiry")
}
func (noopPublisher) PutValue(ctx context.Context, item string, fields map[string]string, cas int) error {
	panic("PutValue should not be called when publish_value is false")
}

type noopVersionReader struct{}

func (noopVersionReader) CurrentVersion(ctx context.Context, item string) (int, bool, error) {
	panic("CurrentVersion should not be called when publish_value is false")
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
	if code := <-exitCode; code != 0 {
		t.Fatalf("cmdPublish exit code = %d, want 0 (the request was served, per design §9); stderr: %s", code, stderrOut.String())
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

// TestNoResponseErrorEverContainsTheRequestValue is this file's own
// redaction proof for the boundary internal/handoff's tests cannot see:
// internal/secrets already proves PatchExpiry and PutValue redact the
// value out of THEIR OWN errors, but publishHandler.handle builds several
// of its own error messages (the wrong-item refusal, the empty-field
// refusal, the vacuous-pass refusal) that never touch internal/secrets at
// all, and any one of them could be written carelessly to interpolate
// req.Value directly. This drives every failure branch handle() has with a
// sentinel-shaped value and asserts it never appears in the Response that
// crosses the socket -- the actual boundary a leak would cross to reach a
// log or a Telegram alert.
func TestNoResponseErrorEverContainsTheRequestValue(t *testing.T) {
	const sentinel = "SENTINEL-HANDLER-MUST-NOT-LEAK-4b7e91"

	fv := newFakePublishVault()
	srv := fv.server()
	defer srv.Close()
	fv.patchFail[itemGitHubApp] = http.StatusInternalServerError // force the expiry-patch failure branch

	jwtPath := writePublishJWT(t)
	expiriesPath := writeExpiriesFile(t, map[string]string{itemGitHubApp: "2027-01-01"})

	cases := []handoff.Request{
		{PublishValue: false}, // the "nothing to publish" branch
		{PublishValue: true, Item: "wrong-item", Field: "password", Value: sentinel},          // the wrong-item refusal
		{PublishValue: true, Item: itemCFInfraAdmin, Field: "", Value: sentinel},              // the empty-field refusal
		{PublishValue: true, Item: itemCFInfraAdmin, Field: fieldCFPassword, Value: sentinel}, // the ordinary write path, with the expiry patch failing alongside it
	}

	for i, req := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			socket := filepath.Join(t.TempDir(), "publish.sock")
			getenv := publishEnv(srv.URL, jwtPath, socket, expiriesPath, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			exitCode, stderrOut := runPublishAsync(ctx, getenv)
			waitForPublishSocket(t, socket)

			resp, err := handoff.Send(context.Background(), socket, 5*time.Second, req)
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			if code := <-exitCode; code != 0 {
				t.Fatalf("cmdPublish exit code = %d, want 0; stderr: %s", code, stderrOut.String())
			}

			if strings.Contains(resp.Error, sentinel) {
				t.Errorf("resp.Error contains the request value: %q", resp.Error)
			}
			for _, s := range resp.Skipped {
				if strings.Contains(s, sentinel) {
					t.Errorf("resp.Skipped entry contains the request value: %q", s)
				}
			}
			encoded, _ := json.Marshal(resp)
			if strings.Contains(string(encoded), sentinel) {
				t.Errorf("the encoded Response contains the request value: %s", encoded)
			}
		})
	}
}
