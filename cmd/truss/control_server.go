package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/plan"
)

// controlBindHost is the control listener's bind address, and the ONLY
// place its value may be written in this repository -- scripts/leakscan
// carries a narrow exemption pinned to this exact declaration, so every
// other reference to it, in code or in a comment, uses this constant
// rather than repeating the literal.
//
// Compiled in, not configurable: the design's whole authz argument is
// that this listener binds the loopback address only, which is a fact
// about the address, not a flag -- the same precedent
// internal/ledger/store.go's isLoopback states for itself. A config value
// permitting a wider bind is exactly the misconfiguration this exists to
// make impossible.
const controlBindHost = "127.0.0.1"

const (
	controlReadHeaderTimeout = 5 * time.Second
	// force-unlock shells `tofu init` then `tofu force-unlock`, which can
	// legitimately take tens of seconds -- longer than /metrics needs.
	controlWriteTimeout   = 90 * time.Second
	controlIdleTimeout    = 60 * time.Second
	controlMaxHeaderBytes = 16 << 10
	// controlMaxBody bounds a request body the same way
	// internal/handoff/handoff.go's maxMessageSize does, and for the same
	// reason: a caller that goes wrong must not be able to make this
	// listener allocate without limit.
	controlMaxBody = 64 * 1024
)

// controlResponse is every route's response shape, following
// internal/handoff's own precedent: a typed vocabulary rather than a
// re-invented spelling per route, and Detail set ALONGSIDE Value rather
// than instead of it, so a refusal always carries a sentence naming what
// to do about it.
type controlResponse struct {
	Value  string `json:"value"`
	Detail string `json:"detail,omitempty"`
}

const (
	controlValueEnqueued      = "enqueued"
	controlValueAlreadyQueued = "already-queued"
	controlValueSkipped       = "skipped"
	controlValueUnlocked      = "unlocked"
)

// controlDeps is what every route needs. e is passEnv (apply_cmd.go):
// env-only fields that cannot go stale, so a route may read e.Cfg,
// e.Dir and e.Getenv directly without asking the loop for them.
type controlDeps struct {
	e    passEnv
	snap *snapshots
	// runNow is a buffered (size 1) channel runLoop also selects on.
	// Sending is non-blocking and coalescing: a second run-now while one
	// is already queued sends nothing, which is what makes "at most one
	// pending manual run" true without a separate queue structure.
	runNow chan struct{}
}

// writeJSON writes a controlResponse with the given status code.
func writeJSON(w http.ResponseWriter, status int, resp controlResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func refuse(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, controlResponse{Value: "refused", Detail: detail})
}

// requireControlToken checks the Authorization header against the token
// mounted at itemControlToken/fieldControlToken, read fresh on every
// request -- the same per-request freshness buildPass gives every other
// credential, so a rotated token takes effect on the very next call rather
// than requiring a restart.
//
// ⚠️ CONSTANT-TIME COMPARISON, NOT ==. A token check that returns faster
// on a wrong first byte than a wrong last byte is a timing side-channel;
// crypto/subtle.ConstantTimeCompare closes it at the cost nothing else
// here needs to pay.
func requireControlToken(e passEnv, r *http.Request) (ok bool, reason string) {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) {
		return false, "missing Authorization: Bearer <token>"
	}
	got = strings.TrimPrefix(got, prefix)

	want, err := e.Dir.Field(itemControlToken, fieldControlToken)
	if err != nil {
		return false, "control token is not mounted"
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		return false, "token does not match"
	}
	return true, ""
}

// controlAuth wraps a handler with the token check and three cheap gates
// against what an operator's own BROWSER can reach once a port-forward is
// open. "RBAC gates who may open a port-forward" says nothing about what a
// web page loaded in that operator's browser can then send to
// localhost:<port> -- a cross-origin simple form post needs no CORS
// preflight, and DNS rebinding defeats a naive Host check.
//
//  1. Require Content-Type: application/json. A cross-origin simple
//     request can only carry application/x-www-form-urlencoded,
//     multipart/form-data or text/plain -- requiring JSON forces a
//     preflight, which a browser will not send without CORS headers this
//     server never returns.
//  2. Refuse any request carrying an Origin header. No legitimate client
//     of this API is a browser.
//  3. Refuse a Host header that is not loopback, closing DNS rebinding
//     (an attacker's domain resolving to the bind address with
//     Host: evil.example).
func controlAuth(e passEnv, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" {
			refuse(w, http.StatusBadRequest, "requests carrying an Origin header are refused; no legitimate caller of this API is a browser")
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host != controlBindHost && host != "localhost" && host != "::1" {
			refuse(w, http.StatusBadRequest, fmt.Sprintf("Host %q is not loopback", r.Host))
			return
		}
		if r.Method == http.MethodPost && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			refuse(w, http.StatusBadRequest, "Content-Type: application/json is required")
			return
		}
		if ok, reason := requireControlToken(e, r); !ok {
			refuse(w, http.StatusUnauthorized, reason)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, controlMaxBody)
		next(w, r)
	}
}

// newControlServer builds the control listener: run-now, skip, status,
// force-unlock. Never http.DefaultServeMux, for the same pprof-exposure
// reason newMetricsServer (metrics_server.go) gives for itself.
func newControlServer(deps controlDeps) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run-now", controlAuth(deps.e, deps.handleRunNow))
	mux.HandleFunc("POST /skip/{sha}", controlAuth(deps.e, deps.handleSkip))
	mux.HandleFunc("GET /status", controlAuth(deps.e, deps.handleStatus))
	mux.HandleFunc("POST /force-unlock", controlAuth(deps.e, deps.handleForceUnlock))

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: controlReadHeaderTimeout,
		WriteTimeout:      controlWriteTimeout,
		IdleTimeout:       controlIdleTimeout,
		MaxHeaderBytes:    controlMaxHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
}

// listenControl mirrors listenMetrics: bind synchronously, so a failure
// to bind is a refusal to start rather than an error delivered to a
// goroutine nobody reads.
func listenControl(ctx context.Context, srv *http.Server, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("control: could not bind %s: %w", addr, err)
	}
	go srv.Serve(ln)
	return nil
}

// handleRunNow enqueues one pass, coalescing: a run-now while one is
// already queued reports the same success rather than a second pass.
// It never runs the pass on the request goroutine -- a slow or
// disconnected HTTP client must never be able to hold the loop.
func (d controlDeps) handleRunNow(w http.ResponseWriter, r *http.Request) {
	select {
	case d.runNow <- struct{}{}:
		writeJSON(w, http.StatusAccepted, controlResponse{Value: controlValueEnqueued})
	default:
		writeJSON(w, http.StatusOK, controlResponse{Value: controlValueAlreadyQueued, Detail: "a manual run is already queued"})
	}
}

// handleStatus answers what the loop knows about itself. ⚠️ DELIBERATELY
// MINIMAL: next-tick and last-outcome detail are not here yet -- both
// need state this change does not build (a scheduled-tick clock reachable
// from outside runLoop, and a structured per-pass Outcome rather than a
// raw metrics.Set). What is here is read from the same snapshots
// /metrics holds, so the two routes cannot disagree.
func (d controlDeps) handleStatus(w http.ResponseWriter, r *http.Request) {
	d.snap.mu.RLock()
	started := d.snap.started
	hasFrequent := d.snap.frequent != nil
	hasDrift := d.snap.drift != nil
	d.snap.mu.RUnlock()

	writeJSON(w, http.StatusOK, controlResponse{
		Value: "ok",
		Detail: fmt.Sprintf("uptime=%s in_flight=%v has_run_frequent=%v has_run_drift=%v",
			time.Since(started).Round(time.Second), d.snap.inFlight.Load(), hasFrequent, hasDrift),
	})
}

// skipRequest is POST /skip/{sha}'s body. confirm_sha must equal the path
// segment -- the sha named twice, from two independent places in the
// request -- and expect_head must equal the ledger's CURRENT HEAD at the
// moment of the write, making this a compare-and-swap.
//
// ⚠️ THIS REPLACES TRUSS_SKIP_I_UNDERSTAND, NOT REIMPLEMENTS IT AS A BODY
// FIELD. skip_cmd.go's guard 4 is an environment variable specifically
// because "a flag is something a script can pass reflexively on every
// invocation, but typing the sha into an environment variable by hand is
// a deliberate act a script blindly retrying cannot forge by accident."
// A body field with the same sha the URL already names would be exactly
// that reflexive flag. expect_head is strictly stronger: a caller cannot
// fill it in without having just read status, and it closes a race the
// CLI does not even have today (HEAD moving between the read and the
// write).
type skipRequest struct {
	Reason     string `json:"reason"`
	ConfirmSHA string `json:"confirm_sha"`
	ExpectHead string `json:"expect_head"`
}

func (d controlDeps) handleSkip(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if sha == "" {
		refuse(w, http.StatusBadRequest, "a commit sha is required in the path: POST /skip/<sha>")
		return
	}

	// ⚠️ CHECKED BEFORE ANYTHING ELSE. Advancing HEAD while a pass may be
	// mid-commit-loop on that very sha is editing memory the pass is
	// writing. This is the control API's own fifth guard, and the reason
	// the route exists at all beyond what the CLI can already do from a
	// laptop with no cluster access.
	if d.snap.inFlight.Load() {
		refuse(w, http.StatusConflict, "a pass is currently running; skip refuses while one is in flight")
		return
	}

	var req skipRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, "body did not parse as JSON: "+err.Error())
		return
	}
	if req.ConfirmSHA != sha {
		refuse(w, http.StatusBadRequest, "confirm_sha must equal the sha named in the path")
		return
	}
	if req.ExpectHead == "" {
		refuse(w, http.StatusBadRequest, "expect_head is required: name the HEAD you read before asking to skip past it")
		return
	}

	ctx := r.Context()
	store, err := buildLedgerStore(d.e.Cfg)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	journal := &ledger.Journal{Store: store, Layout: layoutFor(d.e.Cfg)}

	head, err := journal.Head(ctx)
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "could not read HEAD: "+err.Error())
		return
	}
	if head != req.ExpectHead {
		refuse(w, http.StatusUnprocessableEntity, fmt.Sprintf("expect_head %q does not match the ledger's current HEAD %q; HEAD moved since you last read it", req.ExpectHead, head))
		return
	}

	trimmed := strings.TrimSpace(req.Reason)
	if outcome := checkSkipGuards(ctx, journal, sha, trimmed); outcome.Refused != "" {
		status := http.StatusUnprocessableEntity
		if outcome.NeedsLedger {
			status = http.StatusServiceUnavailable
		}
		refuse(w, status, outcome.Refused)
		return
	}

	// ⚠️ THE LIVE TELEGRAM CREDENTIAL, NOT ONE CAPTURED AT LOOP STARTUP.
	// buildPass reloads it every pass for the same reason (apply_cmd.go);
	// a handler holding a startup copy would announce with a token that
	// may have rotated out hours ago.
	tg, err := loadTelegram(d.e.Dir, d.e.Getenv("TELEGRAM_API_BASE_URL"))
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "could not load the alert credentials: "+err.Error())
		return
	}
	if outcome := finishSkip(ctx, journal, tg, sha, trimmed); outcome.Refused != "" {
		status := http.StatusUnprocessableEntity
		if outcome.NeedsLedger {
			status = http.StatusServiceUnavailable
		}
		refuse(w, status, outcome.Refused)
		return
	}

	writeJSON(w, http.StatusOK, controlResponse{Value: controlValueSkipped, Detail: fmt.Sprintf("skipped %s: %s", sha, trimmed)})
}

// forceUnlockRequest is POST /force-unlock's body. lock_id is required and
// never defaulted -- force-unlocking "whatever lock is there" is how a
// healthy concurrent apply gets broken, and the only way to know the real
// lock ID is to have read a real error message naming it.
type forceUnlockRequest struct {
	Root   string `json:"root"`
	LockID string `json:"lock_id"`
}

// handleForceUnlock is the most dangerous route in this listener --
// only the daemon knows whether a pass is running, which is the entire
// reason this exists on the control API rather than as a standalone
// script (applier/force-unlock refuses while any applier pod is Running,
// which a StatefulSet makes permanently true).
//
// ⚠️ THE IN-FLIGHT CHECK IS A CHECK, NOT A FENCE. Without a Queue
// primitive to run this exclusively on (not built in this change), there
// is a narrow window between this check and the tofu invocation below
// where a scheduled pass could start. Disclosed rather than silently
// accepted: closing it needs the same exclusion mechanism a real run-now
// queue would provide, which is future work.
func (d controlDeps) handleForceUnlock(w http.ResponseWriter, r *http.Request) {
	var req forceUnlockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		refuse(w, http.StatusBadRequest, "body did not parse as JSON: "+err.Error())
		return
	}
	if req.LockID == "" {
		refuse(w, http.StatusBadRequest, "lock_id is required; read the real error tofu printed and name the exact ID")
		return
	}
	if req.Root == "" || strings.Contains(req.Root, "..") || filepath.IsAbs(req.Root) {
		refuse(w, http.StatusBadRequest, "root must be a relative path with no .. segment")
		return
	}
	rootDir := filepath.Join(d.e.Cfg.Workdir, req.Root)
	if info, err := os.Stat(rootDir); err != nil || !info.IsDir() {
		refuse(w, http.StatusBadRequest, fmt.Sprintf("root %q is not a directory in the current checkout", req.Root))
		return
	}
	if d.snap.inFlight.Load() {
		refuse(w, http.StatusConflict, "a pass is currently running; force-unlock refuses while one is in flight")
		return
	}

	tg, err := loadTelegram(d.e.Dir, d.e.Getenv("TELEGRAM_API_BASE_URL"))
	if err != nil {
		refuse(w, http.StatusServiceUnavailable, "could not load the alert credentials: "+err.Error())
		return
	}
	announce := fmt.Sprintf("%s: FORCE-UNLOCK %s lock %s by hand", defaultAlertSubject, req.Root, req.LockID)
	if err := tg.Send(r.Context(), announce); err != nil {
		refuse(w, http.StatusServiceUnavailable, "could not announce the force-unlock: "+err.Error())
		return
	}

	baseEnv := []string{"PATH=" + d.e.Getenv("PATH"), "HOME=" + d.e.Getenv("HOME")}
	runner := plan.Runner{Bin: "tofu", PluginDir: d.e.Cfg.PluginDir, Stderr: d.e.Stderr, Env: baseEnv}
	if err := runner.ForceUnlock(r.Context(), rootDir, req.LockID); err != nil {
		refuse(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, controlResponse{Value: controlValueUnlocked, Detail: fmt.Sprintf("%s unlocked at %s", req.Root, req.LockID)})
}
