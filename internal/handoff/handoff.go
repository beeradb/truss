// Package handoff is the one-shot request/response channel between the
// truss container and the publisher container, over a Unix domain socket
// on a memory-backed emptyDir (design doc §3, "THE HANDOFF").
//
// It is request/RESPONSE, deliberately, and that is load-bearing rather
// than a style choice: truss owns the heartbeat, the ledger record and the
// Telegram alert, so a mint that cannot be published must not be able to
// report success, which requires the verdict to travel back on the same
// channel that carried the value. A one-way pipe cannot do that.
//
// Standard library only -- net, encoding/json, io, time -- so truss keeps
// zero new dependencies for this.
package handoff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

// Value is the vocabulary Response.Value is drawn from. Defined once, here,
// so a caller on either side of the socket does not invent its own spelling
// of "written" or "failed".
const (
	ValueWritten     = "written"
	ValueUnchanged   = "unchanged"
	ValueSkipped     = "skipped"
	ValueFailed      = "failed"
	ValueUnavailable = "unavailable"
)

// maxMessageSize bounds what either side of the socket will read in one
// message -- design §3's "Bounded read": a truss (or, symmetrically, a
// publisher) that goes wrong must not be able to make its peer allocate
// without limit.
const maxMessageSize = 64 * 1024

// connDeadline bounds how long either side waits on read or write once a
// connection exists. It is not the accept-side wait (that is Serve's own
// wait parameter); it is how long one already-established exchange may
// take, and 30s matches the timeout internal/secrets already uses for a
// single Vault HTTP call.
const connDeadline = 30 * time.Second

// Request is what truss hands the publisher, exactly once per apply pass,
// on every code path -- gate failed, drift run, lock contended, rotation
// skipped, rotation applied all end by sending one of these, with
// PublishValue false where there is nothing to publish.
//
// It carries no secret and no data. The publisher fetches the credential
// and its expiry itself, from 1Password, under an item and field name
// compiled into the publisher binary (cmd/truss's itemCFInfraAdmin,
// fieldCFPassword) -- never named by, or received from, this request. A
// bool has nothing to redact, which is why there is no String() method
// here any more: this type used to also carry Item, Field, Value and
// Expires, with a String() built solely to keep Value out of a stray %v,
// and both went together once the publisher stopped needing a value handed
// to it at all.
type Request struct {
	// PublishValue is true exactly when credentials/ was applied AND
	// succeeded on this (drift) pass -- the signal that a freshly minted
	// value exists for the publisher to fetch and write. False on every
	// other pass: the gate failed, rotation was skipped (no credentials
	// root at this commit, or the state lock was held elsewhere), or
	// rotation itself failed.
	PublishValue bool `json:"publish_value"`
}

// Response is what the publisher hands back. There is no field here that
// can hold a credential -- TestTheResponseHasNoFieldThatCanHoldACredential
// marshals one built while a known value was in flight and asserts the
// value's bytes are absent from the encoded form.
type Response struct {
	// Value is one of the Value* constants above.
	Value string `json:"value"`
	// Expiries is how many table entries (plus the minted item's own
	// expiry, when present) were successfully patched this pass.
	Expiries int `json:"expiries"`
	// Skipped names items whose expiry patch failed, and why -- names and
	// reasons, never values.
	Skipped []string `json:"skipped,omitempty"`
	// Error is set whenever something went wrong, even alongside a
	// non-empty Value and Expiries: a partial pass reports both its
	// findings and its error, never one at the expense of the other.
	Error string `json:"error,omitempty"`
}

// Serve listens on a Unix domain socket at path and answers requests until
// ctx is cancelled: accept, read and decode one bounded Request, call h,
// write the Response, close, accept again.
//
// ⚠️ IT USED TO ANSWER EXACTLY ONE, AND THIS PACKAGE'S DOC CALLED IT A
// RENDEZVOUS RATHER THAN A SERVER. That was right while the publisher was
// a sidecar in a CronJob pod: one pass, one request, one process. The
// applier is a daemon now and its publisher is a daemon beside it, so a
// Serve that returned after the first drift pass would leave 364 days of
// the year with nobody listening -- and the failure mode is silence,
// which is the one thing this system is built to refuse.
//
// ⚠️ THE `wait` PARAMETER IS GONE, DELIBERATELY, AND ITS JOB MOVED. It
// existed so the publisher's own exit code could mean "truss died without
// ever asking". Under the loop the interval between requests is a DAY by
// design, so a timeout long enough to be correct would be far too long to
// be a useful signal -- and the loop's own heartbeat staleness already
// answers the question it was asking, from the side that knows the
// answer.
//
// Requests are served ONE AT A TIME, in the order they are accepted:
// there is one drift pass in flight at a time on the other end of this
// socket, and concurrency here would only make two Vault logins race for
// one credential-store version.
//
// The publisher acts only on a request: h is never called until a request
// has arrived and been decoded, so a pass with nothing to publish costs
// this package nothing beyond holding a socket open.
func Serve(ctx context.Context, path string, h func(Request) Response) error {
	// A stale socket file from a previous, uncleanly-killed process would
	// otherwise make Listen fail with "address already in use" -- and the
	// volume this runs on is emptyDir, so nothing else could have created
	// it but a prior instance of this same process.
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("handoff: removing a stale socket at %s: %w", path, err)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("handoff: listening on %s: %w", path, err)
	}
	// ctx cancellation is the only way this loop ends: closing the
	// listener from a separate goroutine is what makes the blocking
	// Accept below return, since net.Listener has no ctx-aware Accept.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				// A closed-listener error caused BY the cancellation above
				// is expected shutdown, not a failure to report.
				return nil
			default:
				return fmt.Errorf("handoff: accepting a connection on %s: %w", path, err)
			}
		}
		// A malformed request on ONE connection must not stop the
		// publisher serving the next -- today it returned, which was
		// fine when there was no "next". Narrated to stderr rather than
		// treated as fatal.
		if err := serveOne(conn, h); err != nil {
			fmt.Fprintf(os.Stderr, "handoff: %v\n", err)
		}
	}
}

// serveOne carries out one request/response exchange on an already-accepted
// connection and always closes it before returning.
func serveOne(conn net.Conn, h func(Request) Response) error {
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(connDeadline)); err != nil {
		return fmt.Errorf("handoff: setting a deadline on the connection: %w", err)
	}

	// The client (Send) writes its request and then closes the write half
	// of the connection, so reading to EOF here reads exactly one message
	// with no length-prefix protocol needed. io.LimitReader bounds it: a
	// request over the limit is truncated rather than exhausting memory,
	// and the truncated read (which will not parse as a Request truss
	// actually sent, since it is missing whatever came after the cut) is
	// refused below rather than ever handed to h.
	limited := io.LimitReader(conn, maxMessageSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("handoff: reading the request: %w", err)
	}
	if len(data) > maxMessageSize {
		return fmt.Errorf("handoff: refusing a request larger than %d bytes", maxMessageSize)
	}

	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("handoff: parsing the request: %w", err)
	}

	resp := h(req)

	encoded, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("handoff: encoding the response: %w", err)
	}
	if _, err := conn.Write(encoded); err != nil {
		return fmt.Errorf("handoff: writing the response: %w", err)
	}
	return nil
}

// Send dials the socket at path, sends r, and returns the publisher's
// verdict. Every return path is meant to be distinguishable to a human:
//
//   - a dial failure reads as "no publisher was listening", never as a
//     Vault error -- because from here, none has happened yet;
//   - a failure to read a response after the request was fully sent reads
//     as "the publisher died mid-publish", which is a different fact than
//     "there was no publisher at all";
//   - a Response with Error set is the publisher itself reporting that it
//     tried and something (typically Vault) refused.
func Send(ctx context.Context, path string, timeout time.Duration, r Request) (Response, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return Response{}, fmt.Errorf("handoff: no publisher listening at %s: %w", path, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return Response{}, fmt.Errorf("handoff: setting a deadline on the connection: %w", err)
	}

	if err := json.NewEncoder(conn).Encode(r); err != nil {
		return Response{}, fmt.Errorf("handoff: sending the request: %w", err)
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		if err := uc.CloseWrite(); err != nil {
			return Response{}, fmt.Errorf("handoff: closing the write side of the connection: %w", err)
		}
	}

	limited := io.LimitReader(conn, maxMessageSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return Response{}, fmt.Errorf("handoff: the publisher died before answering: %w", err)
	}
	if len(data) > maxMessageSize {
		return Response{}, fmt.Errorf("handoff: the publisher's response exceeded %d bytes", maxMessageSize)
	}
	if len(data) == 0 {
		return Response{}, fmt.Errorf("handoff: the publisher closed the connection without answering -- it refused the request rather than serving it")
	}

	var resp Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return Response{}, fmt.Errorf("handoff: parsing the publisher's response: %w", err)
	}
	return resp, nil
}
