// Package ledger is the client for the applier's state bucket: the
// applied/, failed/ and plan-digest records, the head-of-queue pointer and
// the heartbeat. It signs its own requests (SigV4, hand-rolled, no
// third-party dependency) rather than taking an SDK, because an SDK default
// flip is exactly the kind of thing that broke this bucket on 2026-09-06 --
// see the comment on Config.Region and the one above checksum-header
// avoidance in store.go.
package ledger

import (
	"net/http"
	"time"
)

// AddressingStyle selects how the bucket name is folded into the request:
// as the first path segment (PathStyle) or as a subdomain of the endpoint
// host (VirtualHostStyle). PathStyle is the zero value because it is the
// style measured against the real bucket on 2026-09-08 -- an unset Config
// gets the addressing that is known to work, not a guess.
type AddressingStyle int

const (
	// PathStyle builds https://<endpoint>/<bucket>/<key>. This is what the
	// production pod's requests actually resolved to: ForcePathStyle=true,
	// https://storage.googleapis.com/<bucket>/<key>. It is the zero value.
	PathStyle AddressingStyle = iota
	// VirtualHostStyle builds https://<bucket>.<endpoint host>/<key>.
	// Unmeasured against this endpoint; kept for a client that is not this
	// one bucket.
	VirtualHostStyle
)

// Config is everything Store needs to sign and send requests. Endpoint,
// Bucket, AccessKeyID and SecretAccessKey have no defaults: New refuses to
// construct a Store with any of them empty (§4.2, "Refuses to").
type Config struct {
	// Endpoint is the scheme and host, e.g. "https://storage.googleapis.com".
	// No trailing slash.
	Endpoint string
	Bucket   string

	// Region is the string signed into the SigV4 credential scope. Left
	// empty, it defaults to "us-east-1" -- not a real location, but
	// botocore's own fallback: nothing in the reference bash ever set
	// AWS_REGION, so every working production request was signed with
	// whatever botocore defaults to, which is this. Google's S3-compatible
	// API does not read the region to route the request, but the signature
	// covers the string, so changing it is changing what byte sequence gets
	// signed -- it would invalidate every request against the real bucket,
	// not just look wrong. Do not "fix" this to a real region.
	Region string

	// Addressing selects path-style or virtual-host-style URLs. The zero
	// value (PathStyle) is what was measured against the real bucket.
	Addressing AddressingStyle

	AccessKeyID     string
	SecretAccessKey string
}

// defaultRegion is botocore's own fallback -- see the comment on
// Config.Region. It is a constant, not a literal repeated at each call site,
// so there is exactly one place that says what the unmeasured region became.
const defaultRegion = "us-east-1"

// Option configures a Store beyond what Config carries: transport and time,
// both of which a test needs to control and production does not.
type Option func(*Store)

// WithHTTPClient overrides the client used to send requests. Tests use this
// to point a Store at an httptest.Server, or to inject a RoundTripper that
// fails in a specific way (a timeout, a DNS error) without touching a real
// network.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Store) { s.httpClient = c }
}

// WithClock overrides the clock used for SigV4 timestamps. Production never
// calls this; tests that need a reproducible Authorization header do.
func WithClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}
