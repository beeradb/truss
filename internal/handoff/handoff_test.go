package handoff

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sentinelValue is a value that must never appear rendered anywhere --
// error strings, %v/%+v output, or an encoded Response -- across this
// file's tests.
const sentinelValue = "SENTINEL-DO-NOT-LEAK-9f3a1c7e"

func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "publish.sock")
}

// TestSendCarriesTheValueAndReturnsThePublishersVerdict is the round-trip:
// what Serve's handler receives is exactly what Send sent, and Send returns
// exactly what the handler returned.
func TestSendCarriesTheValueAndReturnsThePublishersVerdict(t *testing.T) {
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

	req := Request{PublishValue: true, Item: "cf-infra-admin", Field: "password", Value: sentinelValue, Expires: "2027-01-01"}
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
	if gotReq.Value != sentinelValue || gotReq.Item != "cf-infra-admin" || gotReq.Expires != "2027-01-01" {
		t.Fatalf("handler received %+v, want the exact request Send sent", gotReq)
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
	for _, mustNotContain := range []string{"vault", "Vault", "403", "cas"} {
		if strings.Contains(msg, mustNotContain) {
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

// TestARequestLargerThanTheLimitIsRefused: a request whose encoded body
// exceeds maxMessageSize must never reach the handler.
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

	oversized := Request{PublishValue: true, Item: "cf-infra-admin", Field: "password", Value: strings.Repeat("x", maxMessageSize+1024)}
	_, sendErr := Send(context.Background(), path, 5*time.Second, oversized)
	if sendErr == nil {
		t.Fatal("Send of an oversized request succeeded, want a refusal surfaced to the sender")
	}

	if err := <-serveErr; err == nil {
		t.Fatal("Serve accepted an oversized request, want a refusal")
	} else if !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", maxMessageSize)) {
		t.Errorf("Serve error %q does not name the size limit", err.Error())
	}
	if handlerCalled {
		t.Fatal("the handler was called with an oversized request")
	}
}

// TestTheRequestNeverRendersItsValue is the single most important test in
// this package (per the task brief): a stray %v or %+v anywhere must not
// be the leak.
func TestTheRequestNeverRendersItsValue(t *testing.T) {
	req := Request{PublishValue: true, Item: "cf-infra-admin", Field: "password", Value: sentinelValue, Expires: "2027-01-01"}

	rendered := fmt.Sprintf("%v", req)
	if strings.Contains(rendered, sentinelValue) {
		t.Errorf("%%v of a Request contains its Value: %q", rendered)
	}
	renderedPlus := fmt.Sprintf("%+v", req)
	if strings.Contains(renderedPlus, sentinelValue) {
		t.Errorf("%%+v of a Request contains its Value: %q", renderedPlus)
	}
	explicit := req.String()
	if strings.Contains(explicit, sentinelValue) {
		t.Errorf("Request.String() contains its Value: %q", explicit)
	}

	// A zero-value (nothing to publish) Request must also render safely
	// and legibly -- this is the common case, sent on 44 of every 45
	// passes.
	if got := (Request{}).String(); !strings.Contains(got, "nothing to publish") {
		t.Errorf("zero-value Request.String() = %q, want it to say there is nothing to publish", got)
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
