package inventory

import (
	"strings"
	"testing"
)

// TestAHostWithNoAccessBlockIsAccepted pins the one tolerance in this
// package, so it cannot be tightened by accident and cannot be widened into
// a fallback without somebody deleting a test that says why.
//
// Every host record written before the field existed was, necessarily,
// reached over the tailnet -- it was the only thing the applier could reach
// a machine through. Refusing them all would refuse every commit of every
// deployment that has adopted the inventory, for a field the platform side
// has not written yet. See Host.Access for why reading nil as "tailscale"
// cannot fail open: such a host is still refused when no tailscale
// credential is mounted.
func TestAHostWithNoAccessBlockIsAccepted(t *testing.T) {
	s := validSnapshot()
	h := s.Hosts["alpha"]
	if h.Access != nil {
		t.Fatal("the clean fixture already carries an access block; this test asserts the ABSENT case")
	}
	if got := Check(s); len(got) != 0 {
		t.Fatalf("a host with no access block was refused: %v", got)
	}
}

// TestBothStatedAccessShapesAreAccepted: neither provider is second class.
// A deployment reaching every machine at a stated address is a supported
// way to run truss, not a bootstrap phase, so a fully address-reached
// inventory must be as clean as a fully tailnet-reached one.
func TestBothStatedAccessShapesAreAccepted(t *testing.T) {
	for _, access := range []*Access{
		{Via: AccessTailscale},
		{Via: AccessAddress, Address: "alpha.example.invalid"},
		{Via: AccessAddress, Address: "alpha.example.invalid:2222"},
	} {
		s := validSnapshot()
		h := s.Hosts["alpha"]
		h.Access = access
		s.Hosts["alpha"] = h
		if got := Check(s); len(got) != 0 {
			t.Errorf("access %+v was refused: %v", *access, got)
		}
	}
}

// TestAccessViasIsTheSetCheckEnforces. The list a refusal prints and the
// set the check accepts are the same fact, and two copies of one fact drift
// -- this repository has paid for that with the plan digest and with the
// protection payload. A `via` that Check accepts and AccessVias does not
// name would be a value nobody could discover; one AccessVias names and
// Check refuses would be a refusal recommending an invalid answer.
func TestAccessViasIsTheSetCheckEnforces(t *testing.T) {
	for _, via := range AccessVias() {
		s := validSnapshot()
		h := s.Hosts["alpha"]
		h.Access = &Access{Via: via}
		if via == AccessAddress {
			h.Access.Address = "alpha.example.invalid"
		}
		s.Hosts["alpha"] = h
		if got := Check(s); len(got) != 0 {
			t.Errorf("AccessVias names %q but Check refuses it: %v", via, got)
		}
	}

	s := validSnapshot()
	h := s.Hosts["alpha"]
	h.Access = &Access{Via: "not-a-via"}
	s.Hosts["alpha"] = h
	problems := Check(s)
	if len(problems) != 1 {
		t.Fatalf("want exactly one problem for an unrecognised via, got %v", problems)
	}
	for _, via := range AccessVias() {
		if !strings.Contains(problems[0], via) {
			t.Errorf("the refusal does not offer %q as an answer: %s", via, problems[0])
		}
	}
}

// TestAnAccessBlockDecodesAndAnUnknownFieldInsideItDoesNot. The decoder
// refuses unknown fields precisely so a newer writer's addition is a
// refusal rather than a value this build silently drops -- and the access
// block is the field most likely to grow one, since it is what a second
// provider would extend.
func TestAnAccessBlockDecodesAndAnUnknownFieldInsideItDoesNot(t *testing.T) {
	good := `{"schema":"truss.host/v1","name":"alpha","kind":"vm","role":"dev",
		"provisioned_by":"hosts/alpha","config":"ansible/plays/alpha",
		"tailnet_tags":[],"cluster":null,"frozen":false,"decommissioned":false,
		"access":{"via":"address","address":"alpha.example.invalid:22"}}`
	h, err := DecodeHost("alpha", []byte(good))
	if err != nil {
		t.Fatalf("DecodeHost refused a valid access block: %v", err)
	}
	if h.Access == nil || h.Access.Via != AccessAddress || h.Access.Address != "alpha.example.invalid:22" {
		t.Fatalf("Access decoded as %+v, want via=address with the stated address", h.Access)
	}

	bad := strings.Replace(good, `"address":"alpha.example.invalid:22"`, `"addr":"alpha.example.invalid:22"`, 1)
	if _, err := DecodeHost("alpha", []byte(bad)); err == nil {
		t.Fatal("DecodeHost accepted an unknown field inside the access block; a field this build cannot represent must refuse, not be dropped")
	}
}
