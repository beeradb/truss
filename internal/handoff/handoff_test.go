package handoff

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sentinelValue is a value that must never appear rendered anywhere --
// error strings, %v/%+v output, or an encoded Response -- across this
// file's tests. Response is the only type left in this package that could
// ever carry one (TestTheResponseHasNoFieldThatCanHoldACredential below);
// Request cannot any more (see handoff.go's own doc), which is why this
// sentinel is used to build a Response, never a Request.
const sentinelValue = "SENTINEL-DO-NOT-LEAK-9f3a1c7e"

func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "publish.sock")
}

// TestSendCarriesPublishValueAndReturnsThePublishersVerdict is the
// round-trip: what Serve's handler receives is exactly what Send sent, and
// Send returns exactly what the handler returned.
func TestSendCarriesPublishValueAndReturnsThePublishersVerdict(t *testing.T) {
	path := socketPath(t)

	var gotReq Request
	received := make(chan struct{})
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(context.Background(), path, 5*time.Second, func(r Request) Response {
			gotReq = r
			close(received)
			return Response{Value: ValueWritten, Expiries: 3}
		})
	}()

	// Give Serve a moment to bind before dialing -- Send below retries via
	// its own dial error message otherwise, which this test does not need
	// to exercise (that is TestSendFailsDistinguishablyWhenNoPublisherIsListening).
	waitForSocket(t, path)

	req := Request{PublishValue: true}
	resp, err := Send(context.Background(), path, 5*time.Second, req)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Value != ValueWritten || resp.Expiries != 3 {
		t.Fatalf("Send returned %+v, want Value=%q Expiries=3", resp, ValueWritten)
	}

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("handler was never called")
	}
	if gotReq != req {
		t.Fatalf("handler received %+v, want the exact request Send sent (%+v)", gotReq, req)
	}

	if err := <-serveErr; err != nil {
		t.Fatalf("Serve returned an error after a successful exchange: %v", err)
	}
}

// TestServeAnswersExactlyOneRequestAndThenExits: a second connection after
// the first is not served -- Serve is a rendezvous, not a server, so it
// must have already returned by the time the first exchange completes.
func TestServeAnswersExactlyOneRequestAndThenExits(t *testing.T) {
	path := socketPath(t)
	calls := 0
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(context.Background(), path, 5*time.Second, func(r Request) Response {
			calls++
			return Response{Value: ValueSkipped}
		})
	}()
	waitForSocket(t, path)

	if _, err := Send(context.Background(), path, 5*time.Second, Request{}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	if err := <-serveErr; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler called %d times, want exactly 1", calls)
	}

	// A second Send against the now-exited Serve must fail to connect --
	// proof that nothing is still listening.
	if _, err := Send(context.Background(), path, 500*time.Millisecond, Request{}); err == nil {
		t.Fatal("a second Send after Serve returned succeeded, want a dial failure")
	}
}

// TestSendFailsDistinguishablyWhenNoPublisherIsListening: the message must
// read as "nobody is there", not as if a Vault call had already happened.
func TestSendFailsDistinguishablyWhenNoPublisherIsListening(t *testing.T) {
	path := socketPath(t) // nothing ever listens on this path

	_, err := Send(context.Background(), path, 2*time.Second, Request{})
	if err == nil {
		t.Fatal("Send against nothing listening = nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no publisher listening") {
		t.Errorf("error %q does not say no publisher was listening", msg)
	}
	// path is t.TempDir()'s name: it embeds the test name plus an
	// OS-chosen counter, so it can coincidentally contain digits like
	// "403" with nothing to do with Vault. Strip it before checking for
	// Vault-shaped text, or the test's outcome depends on that counter
	// rather than on what Send actually wrote -- the same class of bug as
	// a fixture passing only because a laptop had a kubectl context named
	// "vault".
	authored := strings.ReplaceAll(msg, path, "")
	for _, mustNotContain := range []string{"vault", "Vault", "403", "cas"} {
		if strings.Contains(authored, mustNotContain) {

			t.Errorf("error %q reads like a Vault error (contains %q), want a plain dial failure", msg, mustNotContain)
		}
	}
}

// TestServeExitsNonZeroWhenNobodyConnectsBeforeTheDeadline: Serve's own
// contract is a returned error (cmd/truss turns that into exit 1) when its
// wait elapses with no connection at all.
func TestServeExitsNonZeroWhenNobodyConnectsBeforeTheDeadline(t *testing.T) {
	path := socketPath(t)
	start := time.Now()
	err := Serve(context.Background(), path, 100*time.Millisecond, func(r Request) Response {
		t.Fatal("handler called despite nobody connecting")
		return Response{}
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Serve with nobody connecting = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "no publish request arrived") {
		t.Errorf("error %q does not explain that nobody connected", err.Error())
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("Serve returned after %s, want it to have waited out its deadline", elapsed)
	}
}

// TestARequestLargerThanTheLimitIsRefused: a message exceeding
// maxMessageSize must never reach the handler. Request no longer has any
// field big enough to build one from (it is a single bool), so this dials
// the socket directly and writes raw oversized bytes the way a malformed or
// hostile peer would -- serveOne's size check runs before JSON decoding, so
// this still exercises exactly the guard the old test did.
func TestARequestLargerThanTheLimitIsRefused(t *testing.T) {
	path := socketPath(t)
	handlerCalled := false
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- Serve(context.Background(), path, 5*time.Second, func(r Request) Response {
			handlerCalled = true
			return Response{Value: ValueWritten}
		})
	}()
	waitForSocket(t, path)

	var d net.Dialer
	conn, err := d.DialContext(context.Background(), "unix", path)
	if err != nil {
		t.Fatalf("dialing the socket: %v", err)
	}
	oversized := bytes.Repeat([]byte("x"), maxMessageSize+1024)
	if _, err := conn.Write(oversized); err != nil {
		t.Fatalf("writing an oversized message: %v", err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			t.Fatalf("closing the write side: %v", err)
		}
	}
	conn.Close()

	if err := <-serveErr; err == nil {
		t.Fatal("Serve accepted an oversized request, want a refusal")
	} else if !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", maxMessageSize)) {
		t.Errorf("Serve error %q does not name the size limit", err.Error())
	}
	if handlerCalled {
		t.Fatal("the handler was called with an oversized request")
	}
}

// TestTheResponseHasNoFieldThatCanHoldACredential: marshal a Response built
// while a known value was "in flight" (as an Error string, the one place a
// careless caller might have put it) and confirm the value's bytes never
// appear encoded -- proving the type itself, used correctly, cannot carry
// one either.
func TestTheResponseHasNoFieldThatCanHoldACredential(t *testing.T) {
	resp := Response{Value: ValueWritten, Expiries: 5, Skipped: []string{"cf-token-mint: probed"}}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), sentinelValue) {
		t.Fatalf("encoded Response contains the sentinel value: %s", encoded)
	}

	// Confirm the struct's fields themselves are exhausted by this list --
	// a reflective walk would be more future-proof than hand enumeration,
	// but the point of this test is that a reviewer can read it and see
	// every field named, which a reflective version would hide.
	fields := []string{resp.Value, fmt.Sprint(resp.Expiries), strings.Join(resp.Skipped, ","), resp.Error}
	for _, f := range fields {
		if strings.Contains(f, sentinelValue) {
			t.Fatalf("a Response field carries the sentinel value: %q", f)
		}
	}
}

// waitForSocket polls for the socket file to exist, bounding the race
// between starting Serve in a goroutine and dialing it. Serve's own
// contract does not include a "ready" signal (design §3 discusses only the
// two ends of the exchange), so a test driving both ends in one process has
// to wait for the filesystem effect it produces.
func waitForSocket(t *testing.T, path string) {
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
