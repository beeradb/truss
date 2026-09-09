package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/gates"
)

// protectionCompliantForNow builds a gates.Protection that clears every bar
// CheckProtection currently checks, built directly against that function
// rather than reused from testsupport_test.go's compliantGatesProtection.
// internal/gates is under concurrent work elsewhere in this tree (AGENTS.md:
// "other agents are editing internal/gates") that has added fields
// (AllowDeletions, RequireLastPushApproval) the shared fixture has not yet
// been updated to satisfy; these ping tests exist to prove the ping feature,
// not to referee that convergence, so they build their own compliant value.
func protectionCompliantForNow() gates.Protection {
	one := 1
	yes := true
	no := false
	return gates.Protection{
		RequiredApprovals:       &one,
		RequireCodeOwners:       &yes,
		DismissStaleReviews:     &yes,
		EnforceAdmins:           &yes,
		AllowForcePushes:        &no,
		AllowDeletions:          &no,
		RequireUpToDateBranch:   &yes,
		RequireLastPushApproval: &yes,
		StatusChecks:            []string{"plan"},
	}
}

// pingRecorder stands in for an external dead-man's-switch monitor
// (Healthchecks.io, Cronitor, ...), recording how many times it was hit.
// These tests exist to prove WHETHER and WHEN the pass pings, not to
// exercise the wire behaviour of the ping itself -- internal/deadman's own
// tests already cover a non-2xx response and a closed server.
type pingRecorder struct {
	mu   sync.Mutex
	hits int
	srv  *httptest.Server
}

func newPingRecorder(t *testing.T, status int) *pingRecorder {
	t.Helper()
	p := &pingRecorder{}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.hits++
		p.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *pingRecorder) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits
}

// idlePassDeps builds a pass with nothing to do: origin/main == last, so
// runCommitLoop's queue is empty and the pass ends with applied=0 noop=0
// failure="" -- notify.Compose's own idle case, "<subject>: nothing to
// apply".
func idlePassDeps(t *testing.T) (applyDeps, *fakeTelegram) {
	t.Helper()
	forgeFake := &fakeForge{ProtectionResult: protectionCompliantForNow()}
	git := &fakeGit{
		CommitsList: nil,
		HasDirFn:    func(string) bool { return false },
	}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, ft := buildTestDeps(t, forgeFake, git, newTofu)
	return deps, ft
}

// failingPassDeps builds a pass that never gets past the branch-protection
// gate -- the simplest failure this binary can produce, matching
// TestApplyAlwaysWritesAHeartbeatAndAlerts.
func failingPassDeps(t *testing.T) (applyDeps, *fakeTelegram) {
	t.Helper()
	forgeFake := &fakeForge{} // zero-value Protection: every pointer nil, refused
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, ft := buildTestDeps(t, forgeFake, git, newTofu)
	return deps, ft
}

// successPassDeps builds a pass that applies exactly one commit at one
// non-credentials root, its plan digest already approved -- reusing
// gateDeps/ourDigest from apply_digest_gate_test.go, the existing fixture
// for a real end-to-end apply.
func successPassDeps(t *testing.T, sha string) (applyDeps, *fakeTelegram) {
	t.Helper()
	deps, fl, ft, _ := gateDeps(t, sha, sha)
	// gateDeps wires its forgeFake's ProtectionResult from
	// testsupport_test.go's compliantGatesProtection, which -- mid
	// concurrent work on internal/gates elsewhere in this tree -- does not
	// yet set every field the current CheckProtection bar checks (see
	// protectionCompliantForNow's own doc). Overriding the same *fakeForge
	// gateDeps already built keeps these tests independent of that
	// fixture's convergence without editing a shared test helper.
	if ff, ok := deps.Forge.(*fakeForge); ok {
		ff.ProtectionResult = protectionCompliantForNow()
	}
	fl.put("digests/"+sha+"/"+gateSlug+".digest", []byte(ourDigest(t)))
	return deps, ft
}

// TestHeartbeatPingFiresOnEveryCompletedPass is requirement 1: every
// completed pass pings the configured monitor exactly once, regardless of
// what the pass found -- success, refusal, or idle alike.
func TestHeartbeatPingFiresOnEveryCompletedPass(t *testing.T) {
	t.Run("successful pass", func(t *testing.T) {
		ping := newPingRecorder(t, http.StatusOK)
		deps, _ := successPassDeps(t, "pingshasuccess")
		deps.Cfg.HeartbeatPingURL = ping.srv.URL

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, "pingshasuccess")

		if result.failure != "" {
			t.Fatalf("result.failure = %q, want empty", result.failure)
		}
		if got := ping.count(); got != 1 {
			t.Fatalf("monitor was hit %d times, want exactly 1", got)
		}
	})

	t.Run("failing pass", func(t *testing.T) {
		ping := newPingRecorder(t, http.StatusOK)
		deps, _ := failingPassDeps(t)
		deps.Cfg.HeartbeatPingURL = ping.srv.URL

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, "headsha1")

		if result.failure == "" {
			t.Fatalf("result.failure is empty, want the branch-protection refusal")
		}
		if got := ping.count(); got != 1 {
			t.Fatalf("monitor was hit %d times, want exactly 1", got)
		}
	})

	t.Run("idle pass", func(t *testing.T) {
		ping := newPingRecorder(t, http.StatusOK)
		deps, _ := idlePassDeps(t)
		deps.Cfg.HeartbeatPingURL = ping.srv.URL

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, "headsha1")

		if result.failure != "" {
			t.Fatalf("result.failure = %q, want empty", result.failure)
		}
		if got := ping.count(); got != 1 {
			t.Fatalf("monitor was hit %d times, want exactly 1", got)
		}
	})
}

// TestIdlePassWithPingURLSendsNoTelegramMessage is requirement 2, the one
// sentence: when a dead-man's-switch is configured, an idle pass pings it
// instead of messaging.
func TestIdlePassWithPingURLSendsNoTelegramMessage(t *testing.T) {
	ping := newPingRecorder(t, http.StatusOK)
	deps, ft := idlePassDeps(t)
	deps.Cfg.HeartbeatPingURL = ping.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, "headsha1")

	if got := ft.lastText(); got != "" {
		t.Fatalf("telegram received %q, want no message sent for an idle pass with a ping URL configured", got)
	}
	if got := ping.count(); got != 1 {
		t.Fatalf("monitor was hit %d times, want exactly 1", got)
	}
}

// TestIdlePassWithoutPingURLStillMessages is the regression guard: every
// existing deployment has no HEARTBEAT_PING_URL configured, so an idle pass
// must keep sending "<subject>: nothing to apply" exactly as it does today.
func TestIdlePassWithoutPingURLStillMessages(t *testing.T) {
	deps, ft := idlePassDeps(t)
	// deps.Cfg.HeartbeatPingURL left at its zero value: "".

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, "headsha1")

	got := ft.lastText()
	if got == "" {
		t.Fatalf("telegram received no message, want the idle summary")
	}
	if !strings.Contains(got, "nothing to apply") {
		t.Fatalf("telegram text = %q, want it to contain %q", got, "nothing to apply")
	}
}

// TestAFailingMonitorDoesNotFailThePass is the non-fatal guarantee: a
// monitor that returns an error status, or is not listening at all, must
// never turn an otherwise-successful (here: idle) pass into a failure, and
// must not resurrect the chat message the idle rule just suppressed. A
// monitor that did not hear from us is the monitor's own problem to alert
// on.
func TestAFailingMonitorDoesNotFailThePass(t *testing.T) {
	t.Run("monitor returns 500", func(t *testing.T) {
		ping := newPingRecorder(t, http.StatusInternalServerError)
		deps, ft := idlePassDeps(t)
		deps.Cfg.HeartbeatPingURL = ping.srv.URL
		var stderr bytes.Buffer
		deps.Stderr = &stderr

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, "headsha1")

		if result.failure != "" {
			t.Fatalf("result.failure = %q, want empty -- a failed ping must not fail the pass", result.failure)
		}
		if got := ft.lastText(); got != "" {
			t.Fatalf("telegram received %q, want no message -- the idle rule does not fall back to messaging on a failed ping", got)
		}
		if !strings.Contains(stderr.String(), "heartbeat ping failed (non-fatal)") {
			t.Fatalf("stderr = %q, want a non-fatal heartbeat ping failure logged", stderr.String())
		}
		if strings.Contains(stderr.String(), ping.srv.URL) {
			t.Fatalf("stderr leaks the monitor URL: %q", stderr.String())
		}
	})

	t.Run("monitor is unreachable", func(t *testing.T) {
		ping := newPingRecorder(t, http.StatusOK)
		closedURL := ping.srv.URL
		ping.srv.Close() // nobody is listening now

		deps, ft := failingPassDeps(t)
		deps.Cfg.HeartbeatPingURL = closedURL
		var stderr bytes.Buffer
		deps.Stderr = &stderr

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, "headsha1")

		if result.failure == "" {
			t.Fatalf("result.failure is empty, want the branch-protection refusal -- unrelated to the ping")
		}
		if got := ft.lastText(); got == "" || !strings.Contains(got, "FAILED") {
			t.Fatalf("telegram text = %q, want the FAILED alert to still be sent", got)
		}
		if !strings.Contains(stderr.String(), "heartbeat ping failed (non-fatal)") {
			t.Fatalf("stderr = %q, want a non-fatal heartbeat ping failure logged", stderr.String())
		}
		if strings.Contains(stderr.String(), closedURL) {
			t.Fatalf("stderr leaks the monitor URL: %q", stderr.String())
		}
	})
}
