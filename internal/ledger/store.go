package ledger

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Store is a client for one bucket, signing every request itself (SigV4,
// hand-rolled -- see the package doc for why). It exposes only Get, Put and
// PutIfAbsent: no Delete, because nothing in this project ever removes a
// ledger entry, and not having the method is what makes that true by
// construction rather than by discipline (§4.2 "Refuses to").
type Store struct {
	cfg        Config
	httpClient *http.Client
	now        func() time.Time
}

// New validates cfg and returns a Store, or refuses. Endpoint, Bucket,
// AccessKeyID and SecretAccessKey must all be non-empty: a Store built on
// any of them being empty would either fail every request or, worse, sign
// requests with an empty secret and produce something that looks like a
// credential problem far from here. Region defaults to "us-east-1" when
// empty (see the comment on Config.Region); Addressing defaults to
// PathStyle, its zero value, which is what was measured against the real
// bucket.
func New(cfg Config, opts ...Option) (*Store, error) {
	var problems []string
	if cfg.Endpoint == "" {
		problems = append(problems, "Endpoint is empty")
	}
	if cfg.Bucket == "" {
		problems = append(problems, "Bucket is empty")
	}
	if cfg.AccessKeyID == "" {
		problems = append(problems, "AccessKeyID is empty")
	}
	if cfg.SecretAccessKey == "" {
		problems = append(problems, "SecretAccessKey is empty")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("ledger: refusing to construct a Store: %s", strings.Join(problems, "; "))
	}

	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if err := checkEndpoint(cfg.Endpoint); err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		cfg.Region = defaultRegion
	}

	s := &Store{
		cfg:        cfg,
		httpClient: http.DefaultClient,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// checkEndpoint refuses an endpoint that is anything other than a scheme
// and a host.
//
// ⚠️ THE SCHEME IS A SECURITY CONTROL, NOT TIDINESS. SigV4 authenticates
// the REQUEST; nothing authenticates the RESPONSE. TLS is the only thing
// establishing that an approved plan digest read back out of this bucket is
// the one CI wrote -- and over http an on-path attacker gets the digest gate
// in two passes, because a mismatch refusal names BOTH digests ("approved
// %s, ours %s") and is then PUT to failed/<sha> over that same plaintext
// channel. The attacker reads our real digest off our own refusal and echoes
// it back as the approved one on the next pass. Found by the 2026-09-08
// security review; the endpoint had been validated for non-emptiness only.
//
// Loopback is exempt because httptest servers are http and are not on a
// network. That is a property of the ADDRESS, not a flag: there is
// deliberately no option to disable this, because a config value permitting
// plaintext is exactly the misconfiguration being prevented.
//
// A query, fragment or userinfo is refused outright rather than ignored.
// requestURL copies the parsed endpoint wholesale, so a RawQuery on it
// reaches the wire while sign hardcodes the canonical query to "" -- signed
// bytes and sent bytes diverging, the same class as the Path/RawPath bug.
// Refusing the input is a smaller rule than teaching two places to agree.
func checkEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("ledger: refusing to construct a Store: Endpoint %q does not parse: %w", endpoint, err)
	}
	var problems []string
	if u.Host == "" {
		problems = append(problems, "it names no host")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		problems = append(problems, fmt.Sprintf("scheme is %q, want https (http is permitted only for loopback)", u.Scheme))
	}
	if u.User != nil {
		problems = append(problems, "it carries userinfo")
	}
	if u.RawQuery != "" {
		problems = append(problems, "it carries a query string, which would reach the wire unsigned")
	}
	if u.Fragment != "" {
		problems = append(problems, "it carries a fragment")
	}
	if u.Path != "" {
		problems = append(problems, fmt.Sprintf("it carries a path (%q); Endpoint is a scheme and a host only", u.Path))
	}
	if len(problems) > 0 {
		return fmt.Errorf("ledger: refusing to construct a Store: Endpoint %q: %s", endpoint, strings.Join(problems, "; "))
	}
	return nil
}

// isLoopback is a fact about the address, deliberately not a configurable
// exemption. A hostname that is not an IP is never loopback here --
// "localhost" resolves through DNS, which is the thing an attacker on the
// path controls.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// requestURL builds the URL and Host header for one key, per cfg.Addressing.
// PathStyle: https://<endpoint>/<bucket>/<key>, Host is the endpoint host.
// VirtualHostStyle: https://<bucket>.<endpoint host>/<key>, Host carries the
// bucket.
func (s *Store) requestURL(key string) (*url.URL, string, error) {
	base, err := url.Parse(s.cfg.Endpoint)
	if err != nil {
		return nil, "", fmt.Errorf("ledger: invalid endpoint %q: %w", s.cfg.Endpoint, err)
	}

	u := *base
	// Path and RawPath are set as a pair, and that is load-bearing rather
	// than belt-and-braces. url.URL.Path holds the DECODED path and
	// URL.String() re-escapes it, so assigning our own percent-encoding to
	// Path alone gets it encoded a second time on the wire -- a key
	// containing a space is signed as %20 and sent as %2520, and the
	// signature then covers a path the server never saw. Setting RawPath
	// makes EscapedPath() hand back our encoding verbatim, so the bytes we
	// sign are the bytes we send. Measured 2026-09-08: without this,
	// "root with space" and "a+b" both mismatch and only unreserved keys
	// work, which is why nothing had caught it.
	switch s.cfg.Addressing {
	case VirtualHostStyle:
		u.Host = s.cfg.Bucket + "." + base.Host
		u.Path = "/" + key
		u.RawPath = "/" + uriEncodePath(key)
	default: // PathStyle
		u.Path = "/" + s.cfg.Bucket + "/" + key
		u.RawPath = "/" + uriEncodePath(s.cfg.Bucket) + "/" + uriEncodePath(key)
	}
	return &u, u.Host, nil
}

// do signs and sends one request, with body already computed so its hash
// can be signed. extraHeaders are attached AND signed -- see sign, which
// covers every header this PACKAGE sets.
//
// ⚠️ "Every header on the wire" would be false, and this comment used to say
// it. net/http.Transport adds Accept-Encoding, User-Agent and Content-Length
// after sign has returned, so those three travel outside SignedHeaders.
// Measured 2026-09-08 by dumping a real request. It is not exploitable --
// the body hash is signed, which bounds Content-Length, and the other two are
// inert -- but the overclaim is what invited a caller to assume more than the
// signature gives.
func (s *Store) do(ctx context.Context, method, key string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	u, host, err := s.requestURL(key)
	if err != nil {
		return nil, err
	}

	// EscapedPath(), not Path: the signature must cover the encoded path
	// that u.String() puts on the wire. See requestURL.
	sig := sign(s.cfg, s.now(), method, host, u.EscapedPath(), body, extraHeaders)

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("ledger: %s %q: %w", strings.ToLower(method), key, err)
	}
	req.Host = host
	req.Header.Set("Host", host)
	req.Header.Set("X-Amz-Content-Sha256", sig.payloadHash)
	req.Header.Set("X-Amz-Date", sig.amzDate)
	req.Header.Set("Authorization", sig.authorization)
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	for k, v := range extraHeaders {
		// collapseSpaces here too, so the bytes SENT are the bytes
		// SIGNED. sign canonicalises the value before hashing it, per
		// SigV4; sending the raw value left the two differing for any
		// value with padding or a double space. Fail-closed (the server
		// canonicalises and would answer SignatureDoesNotMatch), and
		// latent because no caller passes a padded value today -- but it
		// is the same signed-bytes-versus-sent-bytes class as the
		// Path/RawPath bug, and it would have bitten whoever added the
		// next header. Found by the 2026-09-08 code audit.
		req.Header.Set(k, collapseSpaces(v))
	}

	// Deliberately absent, always: X-Amz-Trailer, Content-Encoding:
	// aws-chunked, X-Amz-Checksum-*, X-Amz-Sdk-Checksum-Algorithm. This
	// client never sets them, which is the whole fix -- see the package
	// doc and TestPutSendsNoChecksumTrailer. Google's S3-compatible XML API
	// does not implement trailer encoding and answers a request that
	// carries one with SignatureDoesNotMatch / Invalid argument, which
	// reads like a bad key and is not one.

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ledger: %s %q: %w", strings.ToLower(method), key, err)
	}
	return resp, nil
}

// Get reads a key. It returns ErrNotFound only when the server reports the
// key absent (404 / NoSuchKey); a 403, a 5xx, a timeout, a DNS failure and a
// signature error are each returned as a distinct error naming what
// happened, never mistaken for absence (§3.2). It never returns a nil error
// with an empty body for a key that does not exist.
func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("ledger: get %q: reading response body: %w", key, readErr)
	}

	switch resp.StatusCode {
	case http.StatusOK:
		// Reads strip trailing whitespace, matching the bash's command
		// substitution ($(...) drops trailing newlines) on every caller
		// that read a ledger value into a shell variable.
		return bytes.TrimRight(body, "\r\n\t "), nil
	case http.StatusNotFound:
		return nil, fmt.Errorf("ledger: get %q: %w", key, ErrNotFound)
	default:
		return nil, newRequestError("get", key, resp.StatusCode, body)
	}
}

// Put writes body to key exactly, with no trailing newline added and no
// checksum header attached. It never retries and never re-encodes the body.
func (s *Store) Put(ctx context.Context, key string, body []byte) error {
	resp, err := s.do(ctx, http.MethodPut, key, body, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return newRequestError("put", key, resp.StatusCode, respBody)
}

// There is deliberately no PutIfAbsent, and its absence is a measurement
// rather than an omission. Measured against the real bucket in-cluster on
// 2026-09-08: "If-None-Match: *" is accepted and then IGNORED -- two
// successive writes to the same key both returned 200 and the second
// overwrote the first -- so a create-if-absent built on it would be a guard
// that silently never guards. The native alternative is unreachable under
// this client's auth: "x-goog-if-generation-match: 0" is refused with
//
//	<Code>ExcessHeaderValues</Code>
//	Requests cannot specify both x-amz and x-goog headers.
//
// and SigV4 obliges every request here to carry x-amz-date and
// x-amz-content-sha256. Signing the x-goog header does not help; the refusal
// is about mixing the two families, not about the signature.
//
// So this endpoint offers no create-if-absent primitive to an S3-compatible
// client, and callers must not be handed an API that implies otherwise.
// Whatever needs mutual exclusion takes it from the layer that actually has
// it -- the applier runs one pass at a time and OpenTofu holds a state lock
// -- not from a header this store cannot enforce.
