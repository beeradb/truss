package ledger

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func validTestConfig() Config {
	return Config{
		// A reserved test domain (RFC 2606): New only validates presence
		// here, it never dials out, so this need not resolve.
		Endpoint:        "https://ledger.invalid",
		Bucket:          "a-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "AKIAFAKE",
		SecretAccessKey: "fakesecretfakesecretfakesecret",
	}
}

// TestNewRefusesAnEmptyCredentialOrEndpoint covers §4.2's "Refuses to.
// Construct a Store with any of endpoint, bucket, access key or secret
// empty." Each case zeroes exactly one required field.
func TestNewRefusesAnEmptyCredentialOrEndpoint(t *testing.T) {
	cases := []struct {
		name string
		zero func(*Config)
	}{
		{"endpoint", func(c *Config) { c.Endpoint = "" }},
		{"bucket", func(c *Config) { c.Bucket = "" }},
		{"access key", func(c *Config) { c.AccessKeyID = "" }},
		{"secret key", func(c *Config) { c.SecretAccessKey = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTestConfig()
			tc.zero(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New(%+v) = nil error, want a refusal", cfg)
			}
		})
	}

	// The baseline must actually succeed, or the cases above would be
	// refused for some unrelated reason and prove nothing.
	if _, err := New(validTestConfig()); err != nil {
		t.Fatalf("New(validTestConfig()) = %v, want success", err)
	}
}

// A Region left empty gets botocore's own fallback, "us-east-1" -- not a
// guess, but the value every working production request was already signed
// with (see the comment on Config.Region). Addressing left unset gets
// PathStyle, its zero value, which is the style measured against the real
// bucket.
func TestUnsetRegionAndAddressingGetTheMeasuredDefaults(t *testing.T) {
	cfg := validTestConfig()
	cfg.Region = ""
	// cfg.Addressing left at its zero value.

	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.cfg.Region != "us-east-1" {
		t.Errorf("Region = %q, want the botocore default %q", s.cfg.Region, "us-east-1")
	}
	if s.cfg.Addressing != PathStyle {
		t.Errorf("Addressing = %v, want PathStyle (the zero value)", s.cfg.Addressing)
	}
}

func TestNewReportsEveryProblemNotJustTheFirst(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("New(Config{}) = nil error, want a refusal naming every empty field")
	}
	for _, want := range []string{"Endpoint", "Bucket", "AccessKeyID", "SecretAccessKey"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err.Error(), want)
		}
	}
}

// TestNewRefusesAnEndpointThatIsNotJustASchemeAndAHost guards the fix for
// the 2026-09-08 security review's M2. SigV4 authenticates the REQUEST and
// nothing authenticates the RESPONSE, so TLS is the only thing establishing
// that an approved plan digest read back from this bucket is the one CI
// wrote. Over http the digest gate falls in two passes: a mismatch refusal
// names both digests and is written to failed/<sha> over the same plaintext
// channel, so an on-path attacker reads our digest off our own refusal and
// echoes it back as "approved" next time.
func TestNewRefusesAnEndpointThatIsNotJustASchemeAndAHost(t *testing.T) {
	// Built rather than written as a literal: a userinfo URL is
	// indistinguishable from an email address to scripts/leakscan, which
	// refuses one anywhere in the tree.
	withUserinfo := (&url.URL{
		Scheme: "https",
		User:   url.UserPassword("u", "p"),
		Host:   "ledger.invalid",
	}).String()

	for _, tc := range []struct {
		name, endpoint, want string
	}{
		{"userinfo", withUserinfo, "userinfo"},
		{"plaintext to a real host", "http://ledger.invalid", "want https"},
		{"plaintext to a name that merely looks local", "http://localhost:9000", "want https"},
		{"a query that would reach the wire unsigned", "https://ledger.invalid/?x=1", "query string"},
		{"a fragment", "https://ledger.invalid/#f", "fragment"},
		{"a path", "https://ledger.invalid/some/prefix", "carries a path"},
		{"no host at all", "https://", "names no host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validTestConfig()
			cfg.Endpoint = tc.endpoint
			_, err := New(cfg)
			if err == nil {
				t.Fatalf("New accepted Endpoint %q", tc.endpoint)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestNewAcceptsLoopbackOverPlaintext: the exemption is a fact about the
// ADDRESS, not a flag. An httptest server is plaintext on a loopback address
// and is not on a network; there is deliberately no option to disable the
// scheme check, because a config value permitting plaintext is the
// misconfiguration the check exists to prevent.
//
// The address is taken from a real httptest server rather than written as a
// literal, so this test cannot drift from what the test harness actually
// produces -- and so scripts/leakscan, which refuses a literal IP anywhere in
// the tree, has nothing to refuse.
func TestNewAcceptsLoopbackOverPlaintext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	if !strings.HasPrefix(srv.URL, "http://") {
		t.Fatalf("httptest server URL %q is not plaintext, so this test proves nothing", srv.URL)
	}
	cfg := validTestConfig()
	cfg.Endpoint = srv.URL
	if _, err := New(cfg); err != nil {
		t.Errorf("New(%q) = %v, want success", srv.URL, err)
	}
}
