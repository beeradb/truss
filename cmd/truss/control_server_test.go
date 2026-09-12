package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// controlTestEnv builds a minimal passEnv with a real secrets.Dir carrying
// the control token, so requireControlToken's real file read is under
// test rather than a fake.
func controlTestEnv(t *testing.T, token string) passEnv {
	t.Helper()
	dir, write := testSecretsDir(t)
	if token != "" {
		write(itemControlToken, fieldControlToken, token)
	}
	cfg := testConfig()
	cfg.Workdir = t.TempDir()
	return passEnv{
		Cfg:    cfg,
		Dir:    dir,
		Getenv: func(string) string { return "" },
		Stderr: io.Discard,
	}
}

func doControlRequest(t *testing.T, srv *http.Server, method, path, token string, body []byte, mutate func(*http.Request)) *http.Response {
	t.Helper()
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	req, err := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if mutate != nil {
		mutate(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func TestControlListenerRefusesWithoutAToken(t *testing.T) {
	e := controlTestEnv(t, "the-real-token")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodGet, "/status", "", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestControlListenerRefusesTheWrongToken(t *testing.T) {
	e := controlTestEnv(t, "the-real-token")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodGet, "/status", "wrong-token", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestControlListenerAcceptsTheRealToken(t *testing.T) {
	e := controlTestEnv(t, "the-real-token")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodGet, "/status", "the-real-token", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestControlListenerRefusesARequestCarryingAnOriginHeader(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodGet, "/status", "tok", nil, func(r *http.Request) {
		r.Header.Set("Origin", "https://example.com")
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d: no legitimate caller of this API is a browser", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestControlListenerRefusesANonLoopbackHostHeader(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodGet, "/status", "tok", nil, func(r *http.Request) {
		r.Host = "evil.example:1234"
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d: DNS rebinding must not reach a handler", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestControlListenerRefusesAPostWithoutJSONContentType(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/run-now", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer tok")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /run-now: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestControlListenerRefusesAGetToARunNowRoute(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodGet, "/run-now", "tok", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

func TestRunNowEnqueuesAndCoalescesASecondRequest(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	runNow := make(chan struct{}, 1)
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: runNow})

	resp1 := doControlRequest(t, srv, http.MethodPost, "/run-now", "tok", []byte("{}"), nil)
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first run-now: status = %d, want %d", resp1.StatusCode, http.StatusAccepted)
	}

	resp2 := doControlRequest(t, srv, http.MethodPost, "/run-now", "tok", []byte("{}"), nil)
	defer resp2.Body.Close()
	var body controlResponse
	json.NewDecoder(resp2.Body).Decode(&body)
	if resp2.StatusCode != http.StatusOK || body.Value != controlValueAlreadyQueued {
		t.Fatalf("second run-now: status = %d, value = %q, want 200/%q (coalesced, not a second pass)", resp2.StatusCode, body.Value, controlValueAlreadyQueued)
	}

	select {
	case <-runNow:
	default:
		t.Fatalf("runNow channel is empty, want the first request's send still queued")
	}
}

func TestSkipRefusesWithoutAShaInThePath(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/skip/", "tok", []byte("{}"), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 404 or 400 for a missing sha", resp.StatusCode)
	}
}

func TestSkipRefusesWhenAPassIsInFlight(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	snap.setInFlight(true)
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/skip/deadbeef", "tok", []byte(`{"reason":"x","confirm_sha":"deadbeef","expect_head":"abc"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d: skip must refuse while a pass is running", resp.StatusCode, http.StatusConflict)
	}
}

func TestSkipRefusesWhenConfirmShaDoesNotMatchThePath(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/skip/deadbeef", "tok", []byte(`{"reason":"x","confirm_sha":"somethingelse","expect_head":"abc"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestSkipRefusesWithoutExpectHead(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/skip/deadbeef", "tok", []byte(`{"reason":"x","confirm_sha":"deadbeef"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// TestSkipRefusesWhenExpectHeadIsStale drives the route against a real
// fake ledger, proving the compare-and-swap: a caller naming a HEAD that
// is no longer current is refused with 422, and nothing is written.
func TestSkipRefusesWhenExpectHeadIsStale(t *testing.T) {
	sha := "deadbeef"
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	fl.put("failed/"+sha, []byte(`{"reason":"tofu apply failed","at":"2026-09-08T12:30:45Z"}`))
	fl.put("head", []byte("currenthead"))
	write(itemControlToken, fieldControlToken, "tok")

	cfg := testConfig()
	cfg.SecretsDir = dir.Root
	cfg.Workdir = t.TempDir()
	e := passEnv{
		Cfg:    cfg,
		Dir:    dir,
		Getenv: func(string) string { return "" },
		Stderr: io.Discard,
	}
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/skip/"+sha, "tok",
		[]byte(`{"reason":"known bad plan","confirm_sha":"`+sha+`","expect_head":"stalehead"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnprocessableEntity)
	}
	if _, ok := fl.get("applied/" + sha); ok {
		t.Errorf("applied/%s was written despite a stale expect_head", sha)
	}
}

func TestForceUnlockRefusesWithoutALockID(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/force-unlock", "tok", []byte(`{"root":"platform"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestForceUnlockRefusesATraversalRoot(t *testing.T) {
	e := controlTestEnv(t, "tok")
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/force-unlock", "tok", []byte(`{"root":"../../etc","lock_id":"abc"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d: a root with .. must never reach the filesystem check", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestForceUnlockRefusesARootNotInTheCheckedOutTree(t *testing.T) {
	e := controlTestEnv(t, "tok") // Cfg.Workdir is an empty temp dir
	snap := newSnapshots(time.Unix(0, 0))
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/force-unlock", "tok", []byte(`{"root":"projects/nonexistent","lock_id":"abc"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestForceUnlockRefusesWhileAPassIsInFlight(t *testing.T) {
	e := controlTestEnv(t, "tok")
	if err := os.MkdirAll(filepath.Join(e.Cfg.Workdir, "platform"), 0o755); err != nil {
		t.Fatalf("seeding root dir: %v", err)
	}
	snap := newSnapshots(time.Unix(0, 0))
	snap.setInFlight(true)
	srv := newControlServer(controlDeps{e: e, snap: snap, runNow: make(chan struct{}, 1)})

	resp := doControlRequest(t, srv, http.MethodPost, "/force-unlock", "tok", []byte(`{"root":"platform","lock_id":"abc"}`), nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusConflict)
	}
}
