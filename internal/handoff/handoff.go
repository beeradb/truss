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
// It is never logged: String() exists so that a stray %v or %+v in some
// future error path cannot be the leak (TestTheRequestNeverRendersItsValue
// is the guard).
type Request struct {
	// PublishValue is true exactly when credentials/ was applied this pass
	// AND the minted value differs from the copy already mounted at
	// /secrets/cf-infra-admin/password. Otherwise every other field is
	// absent (omitempty), not merely empty.
	PublishValue bool `json:"publish_value"`
	// Item is the item name to write. Always the one compiled-in name in
	// practice; the publisher refuses anything else regardless.
	Item string `json:"item,omitempty"`
	// Field is the field within Item to write.
	Field string `json:"field,omitempty"`
	// Value is the credential itself. NEVER rendered by String, NEVER
	// logged, and must never reach an error string on either side.
	Value string `json:"value,omitempty"`
	// Expires is the minted item's own expiry, in the same form
	// internal/secrets.LoadExpiries accepts. It travels in the request
	// because it is a fact about THIS mint and only the mint knows it --
	// the authored table (design §4) never carries it.
	Expires string `json:"expires,omitempty"`
}

// String renders a Request for logging without ever including Value. Every
// other field is safe to print: Item and Field are compile-time-constant
// names, Expires is a date, and PublishValue is a bool.
func (r Request) String() string {
	if !r.PublishValue {
		return "publish: nothing to publish this pass"
	}
	return fmt.Sprintf("publish %s/%s (value redacted)", r.Item, r.Field)
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

// Serve listens on a Unix domain socket at path and answers exactly one
// request: it accepts one connection, reads and decodes one Request
// (bounded to maxMessageSize), calls h, encodes and writes the Response,
// and returns. It is not a server; it is a rendezvous (design §3) -- a
// caller that wants to serve a second pass calls Serve again, from a fresh
// process, the way a new pod does.
//
// The publisher acts only on a request: h is never called until a request
// has arrived and been decoded, so a pass with nothing to publish costs
// this package nothing beyond holding a socket open.
//
// If no connection arrives within wait, Serve returns a non-nil error --
// the one case, per the design, where the publisher's own exit code
// carries meaning ("truss died without ever asking").
func Serve(ctx context.Context, path string, wait time.Duration, h func(Request) Response) error {
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
	defer ln.Close()

	type acceptResult struct {
		conn net.Conn
		err  error
	}
	accepted := make(chan acceptResult, 1)
	go func() {
		conn, err := ln.Accept()
		accepted <- acceptResult{conn, err}
	}()

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case res := <-accepted:
		if res.err != nil {
			return fmt.Errorf("handoff: accepting a connection on %s: %w", path, res.err)
		}
		return serveOne(res.conn, h)
	case <-timer.C:
		return fmt.Errorf("handoff: refusing to report success: no publish request arrived in %s -- the applier container did not reach the end of its pass", wait)
	case <-ctx.Done():
		return fmt.Errorf("handoff: %w", ctx.Err())
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
