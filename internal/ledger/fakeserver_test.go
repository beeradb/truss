package ledger

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
)

// fakeBucket is a minimal in-memory stand-in for the real bucket's wire
// behaviour: object storage plus the two things §5.3 says the test double
// must get right by REJECTING what the real endpoint rejects, not by
// asserting a config field.
//
//   - a checksum trailer, aws-chunked encoding or an x-amz-checksum-*/
//     x-amz-sdk-checksum-algorithm header is answered exactly the way
//     Google's S3-compatible API answers one: 400, SignatureDoesNotMatch,
//     "Invalid argument" -- so a test that stops asserting a Go SDK constant
//     and starts asserting wire bytes actually proves something.
//   - a missing key is a 404 with a NoSuchKey body, never an empty 200.
type fakeBucket struct {
	mu        sync.Mutex
	objects   map[string][]byte
	lastReq   *http.Request
	lastBody  []byte
	addressed AddressingStyle // which style requests are expected to arrive in
	bucket    string          // for virtual-host-style Host parsing
}

func newFakeBucket(bucket string, addressing AddressingStyle) *fakeBucket {
	return &fakeBucket{
		objects:   make(map[string][]byte),
		addressed: addressing,
		bucket:    bucket,
	}
}

func (f *fakeBucket) put(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = append([]byte(nil), body...)
}

// keyFromRequest extracts the object key per the configured addressing
// style. Path-style: /<bucket>/<key>. Virtual-host-style: Host is
// "<bucket>.<rest>", path is "/<key>".
func (f *fakeBucket) keyFromRequest(r *http.Request) (key string, ok bool) {
	switch f.addressed {
	case VirtualHostStyle:
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		prefix := f.bucket + "."
		if len(host) <= len(prefix) || host[:len(prefix)] != prefix {
			return "", false
		}
		return trimLeadingSlash(r.URL.Path), true
	default: // PathStyle
		p := trimLeadingSlash(r.URL.Path)
		bp := f.bucket + "/"
		if len(p) <= len(bp) || p[:len(bp)] != bp {
			return "", false
		}
		return p[len(bp):], true
	}
}

func trimLeadingSlash(s string) string {
	if len(s) > 0 && s[0] == '/' {
		return s[1:]
	}
	return s
}

func xmlError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}

func (f *fakeBucket) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastReq = r
	f.mu.Unlock()

	key, ok := f.keyFromRequest(r)
	if !ok {
		xmlError(w, http.StatusBadRequest, "InvalidArgument", "unrecognised bucket in request")
		return
	}

	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		body, exists := f.objects[key]
		f.mu.Unlock()
		if !exists {
			xmlError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(body)

	case http.MethodPut:
		if reason := rejectChecksumHeaders(r); reason != "" {
			xmlError(w, http.StatusBadRequest, "SignatureDoesNotMatch", "Invalid argument: "+reason)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.lastBody = append([]byte(nil), body...)
		// No conditional-write handling, deliberately: the real endpoint
		// accepts "If-None-Match: *" and then overwrites anyway (measured
		// in-cluster 2026-09-08). A fake that enforced the precondition
		// would be stricter than production and would hide exactly that.
		f.objects[key] = append([]byte(nil), body...)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)

	default:
		xmlError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "unsupported method")
	}
}

// rejectChecksumHeaders reports the first reason found to reject the
// request, or "" if it carries none of the headers the real endpoint
// cannot accept (§4.2, the two things the wire demands).
func rejectChecksumHeaders(r *http.Request) string {
	if r.Header.Get("X-Amz-Trailer") != "" {
		return "x-amz-trailer is not supported"
	}
	if r.Header.Get("Content-Encoding") == "aws-chunked" {
		return "aws-chunked encoding is not supported"
	}
	if r.Header.Get("X-Amz-Sdk-Checksum-Algorithm") != "" {
		return "x-amz-sdk-checksum-algorithm is not supported"
	}
	for name := range r.Header {
		if len(name) >= len("X-Amz-Checksum-") && httpCanonicalHasPrefix(name, "X-Amz-Checksum-") {
			return "x-amz-checksum-* is not supported"
		}
	}
	return ""
}

func httpCanonicalHasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// newFakeServerStore starts an httptest.Server backed by a fresh fakeBucket
// and returns a Store pointed at it (PathStyle, the default). The server
// address, not a real host, is what leakscan sees in these tests.
func newFakeServerStore(t testingT, addressing AddressingStyle) (*Store, *fakeBucket, *httptest.Server) {
	t.Helper()
	const bucket = "ledger-test-bucket"
	fb := newFakeBucket(bucket, addressing)
	srv := httptest.NewServer(fb)

	cfg := Config{
		Endpoint:        srv.URL,
		Bucket:          bucket,
		Region:          "us-east-1",
		Addressing:      addressing,
		AccessKeyID:     "AKIAFAKEACCESSKEYID",
		SecretAccessKey: "fakesecretaccesskeyfakesecretaccesskey",
	}

	client := srv.Client()
	if addressing == VirtualHostStyle {
		// The fake bucket's virtual-host name (the bucket as a subdomain of
		// httptest's loopback listener address) does not resolve via real
		// DNS. Redirect every dial to the real listener while leaving the
		// Host header (and thus the request URL this test inspects) alone
		// -- the same technique a hosts-file entry would buy in
		// production, done in-process for a hermetic test.
		realAddr := srv.Listener.Addr().String()
		transport := client.Transport.(*http.Transport).Clone()
		transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, realAddr)
		}
		transport.DialTLSContext = nil
		client = &http.Client{Transport: transport}
	}

	store, err := New(cfg, WithHTTPClient(client))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store, fb, srv
}

// testingT is the subset of *testing.T this helper needs, so it can live in
// a _test.go file without importing "testing" into the non-test build (it
// already only compiles for tests, but keeping the surface narrow makes the
// dependency explicit).
type testingT interface {
	Helper()
	Fatalf(format string, args ...any)
}
