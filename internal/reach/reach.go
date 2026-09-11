// Package reach proves that a declared address answers RIGHT NOW.
//
// It exists because a record is a DECLARATION and evidence is an
// OBSERVATION, and nothing in this repository is allowed to confuse the
// two. An inventory record naming an address says where somebody believes a
// machine is; it says nothing whatever about whether that machine is there.
// Reading the record back as evidence would be the ansible target gate
// failing open -- the precise thing internal/gates exists to forbid -- with
// the added indignity that the declaration would be vouching for itself.
//
// ⚠️ WHAT A PROBE PROVES IS NARROW, AND SAYING SO IS THE POINT. A completed
// TCP connection proves that something accepted a connection on that
// address at that moment. It does not prove the listener is sshd, that the
// applier's key is authorised, or that the machine on the other end is the
// one the record meant. Those are answered by the play's own --check
// pre-run, which runs before any change is made. This answers the one
// question the pre-run cannot answer cheaply and must not be reached
// without: is the machine there at all, before a play starts walking a
// fleet and dies partway through leaving a host half configured.
//
// Nothing here reads the network for content, sends a byte, or retries. It
// dials, and it reports.
package reach

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultPort is the port a probe uses when a record's address names none.
// 22 because a play is run over SSH: ansible's default connection plugin
// execs the local ssh binary, so the port that decides whether a play can
// reach a host is the one sshd listens on. A record wanting another one
// says so -- "<name>:2222" -- rather than this having a knob.
const DefaultPort = "22"

// DefaultTimeout bounds one dial.
//
// ⚠️ IT IS SHORT ON PURPOSE, AND THE TRADE IS THE OPPOSITE OF THE ONE
// tailnetStaleAfter MAKES. That bound is an hour because it judges a
// RECORD of when a device last checked in, where clock skew and a brief
// outage are both cheap to survive. This makes a live connection, in a pass
// that runs unattended every few minutes and must not sit on a dead
// address: a host that is genuinely up answers a TCP handshake in
// milliseconds over any link a play could be run across, so anything beyond
// a few seconds is a machine that is not there yet rather than a machine
// that is slow. Being wrong here is cheap and self-correcting -- the gate
// refuses, the pass fails, and the next pass tries again -- where being
// wrong in the other direction is a play that starts against a host it
// cannot reach.
const DefaultTimeout = 3 * time.Second

// Prober answers whether an address is answering.
type Prober struct {
	// Dial makes one connection. Nil means a real net.Dialer.
	//
	// ⚠️ ITS ABSENCE WOULD BE A BUG, NOT A MISSING CONVENIENCE -- the same
	// finding internal/tailnet.Config.BaseURL records for its own client.
	// Without this seam, every test of the "the host is down" path would
	// have to pick an address nothing answers on and then wait out a real
	// timeout, which is both slow and a test that depends on the machine
	// running it.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)

	// Timeout bounds one dial. Zero means DefaultTimeout.
	Timeout time.Duration
}

// Answers reports whether something is listening on declared right now.
//
// ⚠️ A MALFORMED ADDRESS IS AN ERROR AND A REFUSED CONNECTION IS `false`,
// AND THE TWO MUST NEVER BE REPORTED THE SAME WAY. "this record cannot be
// dialled at all" is a broken declaration somebody has to fix; "nothing
// answered" is a fact about a machine, and the machine might be up again in
// five minutes. A caller that collapsed them would tell an operator to go
// and look at a host when what is actually wrong is a typo in a JSON file.
func (p Prober) Answers(ctx context.Context, declared string) (bool, error) {
	address, err := Address(declared)
	if err != nil {
		return false, err
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dial := p.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}

	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		// Deliberately not wrapped into the caller's refusal: a dial error
		// carries the address and the resolver's own wording, neither of
		// which says anything the caller does not already know, and the
		// refusal an operator reads names the HOST rather than the address
		// it was reached at. Nothing answered is the whole fact.
		return false, nil
	}
	conn.Close()
	return true, nil
}

// Address normalises a record's declared address into something dialable,
// applying DefaultPort when the record names no port.
//
// It is exported and separate from Answers so a caller can find a broken
// record without opening a socket, and so this repository's rule that a
// refusal names what to fix has something to name.
func Address(declared string) (string, error) {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return "", fmt.Errorf("reach: no address to probe: a host reached by a declared address must state one")
	}

	// ⚠️ THE SHAPE IS REFUSED BEFORE net.SplitHostPort IS ASKED, BECAUSE
	// THAT FUNCTION IS FAR MORE FORGIVING THAN IT LOOKS. Handed
	// "ssh://<name>/" it splits on the first colon without complaint and
	// returns host "ssh" with a port of "//<name>/" -- and this function
	// then joined them back into a "normalised" address that dials
	// something else entirely. Measured: it was accepted here until a test
	// asked for it. So anything that is plainly not a host is refused
	// first, and the parser only ever sees candidates.
	if strings.ContainsAny(declared, "/\\ \t") {
		return "", fmt.Errorf("reach: address %q is not a host or host:port -- it must be the machine's address, never a url or a command", declared)
	}

	host, port := declared, ""
	if h, p, err := net.SplitHostPort(declared); err == nil {
		host, port = h, p
	} else if strings.Count(declared, ":") > 1 && !strings.Contains(declared, "[") {
		// A bare IPv6 literal: colons, no port, and net.JoinHostPort is
		// what brackets it. Appending ":22" by hand would produce a string
		// nothing can parse.
		host, port = declared, ""
	}

	if host == "" {
		return "", fmt.Errorf("reach: address %q names a port but no host -- dialling it would reach the applier's own machine, which is the one host a play must never be pointed at by accident", declared)
	}
	if port == "" {
		return net.JoinHostPort(host, DefaultPort), nil
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("reach: address %q does not name a tcp port -- write \"host\" or \"host:port\"", declared)
	}
	return net.JoinHostPort(host, port), nil
}
