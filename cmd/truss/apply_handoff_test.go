package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/handoff"
	"github.com/beeradb/truss/internal/plan"
)

// This file covers the truss side of the publisher handoff
// (publisher-identity-design.md §3, §9): that every return path of a
// non-drift pass contacts the publisher exactly once, that a drift run
// never does, that the request always says "nothing to publish" today, and
// that a response is mapped to a pass failure exactly per §9's table.

// --- §1: HANDOFF_SOCKET is required/forbidden by which pass this is -------

// TestHandoffSocketIsRequiredWhenDriftCheckIsZeroAndForbiddenWhenItIsOne
// pins loadHandoffConfig's fail-closed contract directly: every problem
// reported, nothing defaulted or guessed, matching config.Load's own shape.
func TestHandoffSocketIsRequiredOnTheDriftPassAndForbiddenOnTheFrequentOne(t *testing.T) {
	t.Run("drift pass: required, because only it rotates", func(t *testing.T) {
		getenv := func(name string) string { return "" }
		socket, problems := loadHandoffConfig(getenv, true)
		if socket != "" {
			t.Errorf("socket = %q, want empty when refused", socket)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "HANDOFF_SOCKET") {
			t.Fatalf("problems = %v, want exactly one naming $HANDOFF_SOCKET", problems)
		}

		getenv = func(name string) string {
			if name == "HANDOFF_SOCKET" {
				return "/tmp/publish.sock"
			}
			return ""
		}
		socket, problems = loadHandoffConfig(getenv, true)
		if len(problems) != 0 {
			t.Fatalf("problems = %v, want none when the socket is set", problems)
		}
		if socket != "/tmp/publish.sock" {
			t.Errorf("socket = %q, want the configured path", socket)
		}
	})

	t.Run("frequent pass: forbidden, it has no publisher", func(t *testing.T) {
		getenv := func(name string) string { return "" }
		socket, problems := loadHandoffConfig(getenv, false)
		if socket != "" || len(problems) != 0 {
			t.Fatalf("socket=%q problems=%v, want a clean pass-through when unset on a frequent pass", socket, problems)
		}

		getenv = func(name string) string {
			if name == "HANDOFF_SOCKET" {
				return "/tmp/publish.sock"
			}
			return ""
		}
		socket, problems = loadHandoffConfig(getenv, false)
		if socket != "" {
			t.Errorf("socket = %q, want empty when refused", socket)
		}
		if len(problems) != 1 || !strings.Contains(problems[0], "HANDOFF_SOCKET") || !strings.Contains(problems[0], "frequent") {
			t.Fatalf("problems = %v, want exactly one naming $HANDOFF_SOCKET and the frequent pass", problems)
		}
	})
}

// --- §2: the send happens on every return path, and only there -----------

// TestEveryPassPathContactsThePublisherExactlyOnce is the anti-wedge
// assertion: gate failed, lock contended, the commit loop applied
// something, and the commit loop errored all must contact the publisher
// exactly once -- a truss that finishes without doing so leaves the
// publisher container blocked until its own deadline, and
// concurrencyPolicy: Forbid then silently suppresses every later pass. A
// drift run must never contact it at all, since that pass carries no
// publisher container.
// TestEveryDriftPassPathContactsThePublisherExactlyOnce pins the operational
// hazard design §9 names: a pass that finishes WITHOUT contacting the publisher
// leaves that container waiting until its deadline, and concurrencyPolicy:
// Forbid then silently suppresses every pass after it. So every return path of
// a drift pass must send, including the ones where there is nothing to publish.
//
// ⚠️ THE DRIFT PASS, NOT THE FREQUENT ONE -- and an earlier version of this
// test asserted the exact opposite. Rotation runs only under DRIFT_CHECK=1, so
// the minted value exists only there; wiring the publisher to the frequent pass
// put it on the one pass that never mints anything, publishing nothing forever
// while the pass that does rotate had nobody to hand its value to.
//
// Each case also pins req.PublishValue: "rotation ran and succeeded" (see
// runRotation's applied return) is the ONLY thing that sets it true --
// gate failed, no credentials root, the state lock held elsewhere, and a
// rotation that ran but failed must all send false. Publishing whatever
// rotation last succeeded at, on a pass where THIS attempt failed or never
// ran, would publish a value nothing here has just verified against
// Cloudflare -- named as its own rule in the task brief, not merely an
// implementation detail.
func TestEveryDriftPassPathContactsThePublisherExactlyOnce(t *testing.T) {
	// driftDeps builds a drift pass whose gate passes and whose roots all
	// exist, so rotation and drift both get their chance. Each case then
	// breaks one thing.
	driftDeps := func(t *testing.T, tofu *fakeTofu) applyDeps {
		t.Helper()
		forgeFake := compliantCommitGate("alice", "headsha1", "headsha1")
		forgeFake.ProtectionResult = compliantGatesProtection()
		git := &fakeGit{HasDirFn: func(string) bool { return true }}
		deps, _, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return tofu })
		deps.Cfg.DriftOnly = true
		return deps
	}

	cases := []struct {
		name             string
		build            func(t *testing.T) applyDeps
		wantCalls        int
		wantPublishValue bool // only checked when wantCalls > 0
	}{
		{
			name: "drift: gate failed",
			build: func(t *testing.T) applyDeps {
				forgeFake := &fakeForge{} // zero Protection: refused
				git := &fakeGit{HasDirFn: func(string) bool { return false }}
				deps, _, _ := buildTestDeps(t, forgeFake, git,
					func(env []string) tofuRunner { return &fakeTofu{} })
				deps.Cfg.DriftOnly = true
				return deps
			},
			wantCalls:        1,
			wantPublishValue: false,
		},
		{
			name: "drift: rotation ran cleanly",
			build: func(t *testing.T) applyDeps {
				return driftDeps(t, &fakeTofu{})
			},
			wantCalls:        1,
			wantPublishValue: true,
		},
		{
			name: "drift: rotation failed",
			build: func(t *testing.T) applyDeps {
				// A rotation failure is a pass failure, and the publisher must
				// STILL be contacted -- otherwise a bad night wedges every
				// following pass via concurrencyPolicy: Forbid. It must also
				// never be told to publish: this attempt never verified a
				// value against Cloudflare.
				return driftDeps(t, &fakeTofu{ApplyErr: errors.New("rotation blew up")})
			},
			wantCalls:        1,
			wantPublishValue: false,
		},
		{
			name: "drift: state lock held elsewhere",
			build: func(t *testing.T) applyDeps {
				return driftDeps(t, &fakeTofu{InitErr: plan.ErrLockBusy})
			},
			wantCalls:        1,
			wantPublishValue: false,
		},
		{
			name: "drift: rotation skipped, no credentials root at this commit",
			build: func(t *testing.T) applyDeps {
				forgeFake := compliantCommitGate("alice", "headsha1", "headsha1")
				forgeFake.ProtectionResult = compliantGatesProtection()
				// Every root exists except credentials -- HasDir("credentials")
				// is runRotation's own skip test.
				git := &fakeGit{HasDirFn: func(root string) bool { return root != "credentials" }}
				deps, _, _ := buildTestDeps(t, forgeFake, git, func(env []string) tofuRunner { return &fakeTofu{} })
				deps.Cfg.DriftOnly = true
				return deps
			},
			wantCalls:        1,
			wantPublishValue: false,
		},
		{
			name: "frequent pass: never, it has no publisher",
			build: func(t *testing.T) applyDeps {
				const sha = "commitsha2"
				deps, fl, _, _ := gateDeps(t, sha, sha)
				fl.put("digests/"+sha+"/"+gateSlug+".digest", []byte(ourDigest(t)))
				return deps
			},
			wantCalls: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := tc.build(t)
			fh := &fakeHandoff{Resp: handoff.Response{Value: handoff.ValueSkipped}}
			deps.Handoff = fh.send
			deps.HandoffSocket = "fake-socket"

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			runApplyPass(ctx, deps, "headsha1")

			if got := fh.callCount(); got != tc.wantCalls {
				t.Fatalf("publisher contacted %d time(s), want %d", got, tc.wantCalls)
			}
			if tc.wantCalls > 0 {
				if got := fh.lastRequest().PublishValue; got != tc.wantPublishValue {
					t.Errorf("req.PublishValue = %v, want %v", got, tc.wantPublishValue)
				}
			}
		})
	}
}

// TestNothingToPublishSendsPublishValueFalseAndNoValueField covers the
// FREQUENT pass: DriftOnly defaults false here, so runApplyPass never even
// reaches the `if driftRun` block that contacts the publisher at all --
// fh is never called, and Request's own zero value already means "nothing
// to publish". TestEveryDriftPassPathContactsThePublisherExactlyOnce is
// where PublishValue's real wiring (rotation ran and succeeded, on a DRIFT
// pass) is pinned, case by case.
func TestNothingToPublishSendsPublishValueFalseAndNoValueField(t *testing.T) {
	forgeFake := &fakeForge{}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	fh := &fakeHandoff{Resp: handoff.Response{Value: handoff.ValueSkipped}}
	deps.Handoff = fh.send
	deps.HandoffSocket = "fake-socket"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runApplyPass(ctx, deps, "headsha1")

	if got := fh.callCount(); got != 0 {
		t.Fatalf("publisher contacted %d time(s) on a frequent pass, want 0", got)
	}
	req := fh.lastRequest()
	if req != (handoff.Request{}) {
		t.Fatalf("req = %+v, want the zero Request", req)
	}
}

// --- §3/§9: mapping the response -------------------------------------------

// TestAValuePublishFailureFailsThePassAndIsFiledUnderItsOwnRotationKey and
// its siblings below call runHandoff directly rather than through
// runApplyPass: driving every §9 table row through a real drift pass would
// mean a real rotation success or failure for each one, which is
// apply_rotation_test.go's and apply_credcache_test.go's job, not this
// file's. Handing runHandoff the request directly isolates §9's own
// mapping (a Response, or a Send error, folded to a pass failure or not)
// from how PublishValue came to be true.
func TestAValuePublishFailureFailsThePassAndIsFiledUnderItsOwnRotationKey(t *testing.T) {
	forgeFake := &fakeForge{}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	fh := &fakeHandoff{Resp: handoff.Response{Value: handoff.ValueFailed, Error: "vault: 403"}}
	deps.Handoff = fh.send
	deps.HandoffSocket = "fake-socket"

	req := handoff.Request{PublishValue: true}
	failure := runHandoff(context.Background(), deps, req, "headsha1")

	if failure == "" {
		t.Fatal("failure is empty, want a rotation failure: a minted value could not be published")
	}
}

// TestASendErrorWithAMintedValueAlsoFailsThePass covers §9's second row:
// "a value was minted and no publisher was listening" is folded the same
// way as a publisher-reported error.
func TestASendErrorWithAMintedValueAlsoFailsThePass(t *testing.T) {
	forgeFake := &fakeForge{}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	fh := &fakeHandoff{Err: errors.New("dial unix fake-socket: connect: no such file or directory")}
	deps.Handoff = fh.send
	deps.HandoffSocket = "fake-socket"

	req := handoff.Request{PublishValue: true}
	failure := runHandoff(context.Background(), deps, req, "headsha1")

	if failure == "" {
		t.Fatal("failure is empty, want a rotation failure: nobody was listening for a minted value")
	}
}

// TestAPublishFailureSaysTheApplySucceeded pins §9's exact requirement on
// the sentence: the token WAS minted, Cloudflare DOES have it, 1Password
// and the state DO hold it -- an error that fails to say so is the
// PublishInProgress mistake repeated.
func TestAPublishFailureSaysTheApplySucceeded(t *testing.T) {
	forgeFake := &fakeForge{}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	fh := &fakeHandoff{Resp: handoff.Response{Value: handoff.ValueFailed, Error: "vault: 403"}}
	deps.Handoff = fh.send
	deps.HandoffSocket = "fake-socket"

	req := handoff.Request{PublishValue: true}
	failure := runHandoff(context.Background(), deps, req, "deadbeef")

	if !strings.Contains(failure, "credentials applied at deadbeef") {
		t.Errorf("failure = %q, want it to say the apply succeeded", failure)
	}
	if !strings.Contains(failure, "Vault still holds the previous value") {
		t.Errorf("failure = %q, want it to say Vault still holds the previous value", failure)
	}
}

// TestAnExpiryOnlyPublishFailureIsReportedAndDoesNotFailThePass covers §9's
// "nothing to publish; the expiry patches failed" row: the publisher's own
// error is real, but truss never asked it to publish a value this pass, so
// it must not fail the pass -- the same reasoning already applied to the
// expiry sweep elsewhere in this file.
func TestAnExpiryOnlyPublishFailureIsReportedAndDoesNotFailThePass(t *testing.T) {
	forgeFake := &fakeForge{}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	var stderr bytes.Buffer
	deps.Stderr = &stderr

	fh := &fakeHandoff{Resp: handoff.Response{Value: handoff.ValueFailed, Skipped: []string{"cf-token-mint: 403"}, Error: "cf-token-mint: 403"}}
	deps.Handoff = fh.send
	deps.HandoffSocket = "fake-socket"

	req := handoff.Request{} // nothing to publish
	failure := runHandoff(context.Background(), deps, req, "headsha1")

	if failure != "" {
		t.Fatalf("failure = %q, want empty -- nothing was asked to publish", failure)
	}
	if !strings.Contains(stderr.String(), "cf-token-mint: 403") {
		t.Errorf("stderr = %q, want the expiry-patch failure narrated", stderr.String())
	}
}

// TestASendErrorWithNothingToPublishIsReportedAndDoesNotFailThePass covers
// §9's "nothing to publish; no publisher was listening" row.
func TestASendErrorWithNothingToPublishIsReportedAndDoesNotFailThePass(t *testing.T) {
	forgeFake := &fakeForge{}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	fh := &fakeHandoff{Err: errors.New("dial unix fake-socket: connect: no such file or directory")}
	deps.Handoff = fh.send
	deps.HandoffSocket = "fake-socket"

	failure := runHandoff(context.Background(), deps, handoff.Request{}, "headsha1")
	if failure != "" {
		t.Fatalf("failure = %q, want empty -- nobody was listening, but there was nothing to publish", failure)
	}
}

// TestApplyPartialPublishReportsBothItsWritesAndItsError covers §9's "some
// expiries written, then one failed" row on the truss/runHandoff side
// (publish_cmd_test.go's TestPartialPublishReportsBothItsWritesAndItsError
// covers the publisher's own half of the same contract): a partial result
// must not have its Expiries/Skipped silently dropped in favour of the
// error, or the reverse.
func TestApplyPartialPublishReportsBothItsWritesAndItsError(t *testing.T) {
	forgeFake := &fakeForge{}
	git := &fakeGit{HasDirFn: func(string) bool { return false }}
	newTofu := func(env []string) tofuRunner { return &fakeTofu{} }
	deps, _, _ := buildTestDeps(t, forgeFake, git, newTofu)

	var stderr bytes.Buffer
	deps.Stderr = &stderr

	fh := &fakeHandoff{Resp: handoff.Response{
		Value:    handoff.ValueFailed,
		Expiries: 4,
		Skipped:  []string{"github-app: 403"},
		Error:    "github-app: 403",
	}}
	deps.Handoff = fh.send
	deps.HandoffSocket = "fake-socket"

	failure := runHandoff(context.Background(), deps, handoff.Request{}, "headsha1")
	if failure != "" {
		t.Fatalf("failure = %q, want empty -- nothing was asked to publish", failure)
	}
	out := stderr.String()
	if !strings.Contains(out, "expiries=4") {
		t.Errorf("stderr = %q, want the successful patch count reported", out)
	}
	if !strings.Contains(out, "github-app: 403") {
		t.Errorf("stderr = %q, want the failure reported alongside the successes", out)
	}
}

// --- §4: the apply pass never constructs a publisher identity -------------

// TestTheApplyPassNeverReadsThePublishJWTPath asserts, by parsing the
// source rather than trusting a comment, that apply_cmd.go never mentions
// the publish audience/token path and never builds a Vault role literal
// naming the publisher. This is the whole point of the separate identity
// (publisher-identity-design.md §0, §2): the kubelet never projects that
// token into the truss container, and a code path that tries to read it
// anyway is a bug even though the kubelet would stop it first.
func TestTheApplyPassNeverReadsThePublishJWTPath(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "apply_cmd.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing apply_cmd.go: %v", err)
	}

	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			v, err := strconv.Unquote(lit.Value)
			if err == nil && strings.Contains(v, "vault-publish") {
				t.Errorf("apply_cmd.go contains the string %q, which names the publisher's audience or token path", v)
			}
		}
		if kv, ok := n.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Role" {
				if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, err := strconv.Unquote(lit.Value)
					if err == nil && v == "publisher" {
						t.Errorf("apply_cmd.go sets Role: %q -- the apply pass must never construct a publisher Vault identity", v)
					}
				}
			}
		}
		return true
	})
}
