package reach

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// --- Address ------------------------------------------------------------

// TestAnAddressWithNoPortGetsTheSSHPort: a record naming a bare host is the
// ordinary case, and the port that decides whether a play can reach a
// machine is the one sshd listens on.
func TestAnAddressWithNoPortGetsTheSSHPort(t *testing.T) {
	got, err := Address("dev-agent.example.invalid")
	if err != nil {
		t.Fatalf("Address returned %v, want a normalised address", err)
	}
	if got != "dev-agent.example.invalid:"+DefaultPort {
		t.Fatalf("Address = %q, want the host with the default port appended", got)
	}
}

// TestADeclaredPortIsHonoured: a record that states a port means it, and
// nothing here may quietly replace it with the default.
func TestADeclaredPortIsHonoured(t *testing.T) {
	got, err := Address("dev-agent.example.invalid:2222")
	if err != nil {
		t.Fatalf("Address returned %v", err)
	}
	if got != "dev-agent.example.invalid:2222" {
		t.Fatalf("Address = %q, want the declared port kept", got)
	}
}

// TestAnEmptyAddressIsRefused: a host declared as reached by address, with
// no address, has nothing to be probed at -- and the refusal must say so
// rather than produce a dial that fails for an unrelated-looking reason.
func TestAnEmptyAddressIsRefused(t *testing.T) {
	if _, err := Address("   "); err == nil {
		t.Fatal("Address accepted an empty address; a host reached by address must state one")
	}
}

// TestAnAddressThatIsNotAnAddressIsRefused: the field is where the applier
// DIALS. A url or a command in it is a record nobody can act on, and
// silently dialling the first word of it would be worse than refusing.
func TestAnAddressThatIsNotAnAddressIsRefused(t *testing.T) {
	for _, bad := range []string{"ssh://dev-agent.example.invalid/", "dev-agent.example.invalid ssh"} {
		if _, err := Address(bad); err == nil {
			t.Errorf("Address(%q) was accepted; it is not a host or host:port", bad)
		}
	}
}

// TestAPortWithNoHostIsRefused: ":22" names a port on nothing. Dialling it
// reaches the local machine, which is the one host a play must never be
// pointed at by accident.
func TestAPortWithNoHostIsRefused(t *testing.T) {
	if _, err := Address(":22"); err == nil {
		t.Fatal("Address(\":22\") was accepted; it names a port but no host")
	}
}

// --- Answers ------------------------------------------------------------

// TestALiveListenerAnswers drives the real dialer against a real listener,
// so the ordinary success path is proved by an actual connection rather
// than by a fake agreeing with itself.
func TestALiveListenerAnswers(t *testing.T) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("could not open a listener: %v", err)
	}
	defer l.Close()
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	answered, err := Prober{}.Answers(context.Background(), l.Addr().String())
	if err != nil {
		t.Fatalf("Answers returned %v, want a clean observation", err)
	}
	if !answered {
		t.Fatal("Answers = false against a live listener")
	}
}

// TestNothingAnsweringIsNotAnError is the distinction the whole method
// rests on: a refused connection is a fact about a machine, which might be
// back in five minutes, where an error means the record itself cannot be
// dialled and somebody has to edit a file.
func TestNothingAnsweringIsNotAnError(t *testing.T) {
	p := Prober{Dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}}
	answered, err := p.Answers(context.Background(), "dev-agent.example.invalid")
	if err != nil {
		t.Fatalf("Answers returned %v, want nil -- nothing answering is an observation, not a broken record", err)
	}
	if answered {
		t.Fatal("Answers = true when the dial failed")
	}
}

// TestAMalformedAddressIsAnErrorAndNotSilentlyUnreachable: the opposite
// direction. A typo in a JSON file must not be reported as a host that is
// down, or an operator goes and looks at a machine that is fine.
func TestAMalformedAddressIsAnErrorAndNotSilentlyUnreachable(t *testing.T) {
	dialed := false
	p := Prober{Dial: func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, nil
	}}
	if _, err := p.Answers(context.Background(), ""); err == nil {
		t.Fatal("Answers accepted an empty address")
	}
	if dialed {
		t.Fatal("a malformed address was dialled anyway")
	}
}

// TestTheProbeDialsTheNormalisedAddress pins that the port is applied
// before the dial rather than after: a probe of the wrong port proves
// nothing about whether a play can reach the host.
func TestTheProbeDialsTheNormalisedAddress(t *testing.T) {
	var got string
	p := Prober{Dial: func(_ context.Context, network, address string) (net.Conn, error) {
		got = network + " " + address
		return nil, errors.New("refused")
	}}
	if _, err := p.Answers(context.Background(), "dev-agent.example.invalid"); err != nil {
		t.Fatalf("Answers returned %v", err)
	}
	if got != "tcp dev-agent.example.invalid:"+DefaultPort {
		t.Fatalf("dialled %q, want tcp and the default port", got)
	}
}

// TestADialThatHangsIsBoundedByTheTimeout. An unattended pass that runs
// every few minutes must not sit on a dead address: a host that is up
// answers a handshake in milliseconds, so anything past the bound is a
// machine that is not there.
func TestADialThatHangsIsBoundedByTheTimeout(t *testing.T) {
	p := Prober{
		Timeout: 20 * time.Millisecond,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	// ⚠️ RUN OFF THE TEST GOROUTINE, SO A MISSING BOUND FAILS RATHER THAN
	// HANGS. Called inline, a Prober with no timeout blocks here until the
	// whole package's test binary is killed, which reports as a panic in
	// whatever test happened to be running -- an unreadable failure for the
	// one guard whose entire subject is not waiting forever.
	type result struct {
		answered bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		answered, err := p.Answers(context.Background(), "dev-agent.example.invalid")
		done <- result{answered, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Answers returned %v", got.err)
		}
		if got.answered {
			t.Fatal("Answers = true after the dial timed out")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the probe never returned: an unattended pass must not sit on a dead address")
	}
}

// TestAPortThatIsNotANumberIsRefused. The port is validated rather than
// left to the dialer because the two failures must not be told apart by
// their timing: a typo like ":2222x" would otherwise come back as "this
// host is unreachable", sending an operator to look at a machine that is
// perfectly fine. Numeric only -- a service name would put an /etc/services
// lookup between the reviewed record and the connection for no gain.
func TestAPortThatIsNotANumberIsRefused(t *testing.T) {
	for _, bad := range []string{"dev-agent.example.invalid:2222x", "dev-agent.example.invalid:ssh", "dev-agent.example.invalid:0"} {
		if _, err := Address(bad); err == nil {
			t.Errorf("Address(%q) was accepted; a port that cannot be dialled must refuse rather than read as a host being down", bad)
		}
	}
}

// TestAnUnsetTimeoutUsesTheDefault, so a caller that forgot one gets a
// bound rather than a dial that can hang for as long as the kernel allows.
func TestAnUnsetTimeoutUsesTheDefault(t *testing.T) {
	var deadline time.Time
	p := Prober{Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("no deadline on the dial context")
		}
		deadline = d
		return nil, errors.New("refused")
	}}
	if _, err := p.Answers(context.Background(), "dev-agent.example.invalid"); err != nil {
		t.Fatalf("Answers returned %v", err)
	}
	if deadline.IsZero() {
		t.Fatal("the dial got no deadline; an unset Timeout must fall back to DefaultTimeout")
	}
	if until := time.Until(deadline); until > DefaultTimeout+time.Second {
		t.Fatalf("deadline is %v away, want about DefaultTimeout (%v)", until, DefaultTimeout)
	}
}

// TestAnAnsweredProbeClosesItsConnection: the pass runs every few minutes
// against every declared address, so a leaked descriptor per host per pass
// is an applier that eventually cannot open a socket at all.
func TestAnAnsweredProbeClosesItsConnection(t *testing.T) {
	closed := make(chan struct{}, 1)
	p := Prober{Dial: func(context.Context, string, string) (net.Conn, error) {
		return &closeRecorder{closed: closed}, nil
	}}
	if _, err := p.Answers(context.Background(), "dev-agent.example.invalid"); err != nil {
		t.Fatalf("Answers returned %v", err)
	}
	select {
	case <-closed:
	default:
		t.Fatal("the probe left its connection open")
	}
}

// closeRecorder is a net.Conn that records only that it was closed. Every
// other method panics: nothing in this package may read or write a byte,
// and a test that quietly permitted it would stop saying so.
type closeRecorder struct {
	net.Conn
	closed chan struct{}
}

func (c *closeRecorder) Close() error {
	select {
	case c.closed <- struct{}{}:
	default:
	}
	return nil
}

// TestARefusalNamesWhatTheFieldMustContain: a refusal an operator cannot
// act on is barely better than none, and this field is hand-authored JSON
// where the likeliest error is somebody writing a url into it.
func TestARefusalNamesWhatTheFieldMustContain(t *testing.T) {
	_, err := Address("ssh://dev-agent.example.invalid/")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "host or host:port") {
		t.Fatalf("refusal %q does not say what the field must contain", err)
	}
}
