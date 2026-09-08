package ledger

import (
	"strings"
	"testing"
)

func validTestConfig() Config {
	return Config{
		// A reserved test domain (RFC 2606): New only validates presence
		// here, it never dials out, so this need not resolve.
		Endpoint:        "http://ledger.invalid",
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
