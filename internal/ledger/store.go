package ledger

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	switch s.cfg.Addressing {
	case VirtualHostStyle:
		u.Host = s.cfg.Bucket + "." + base.Host
		u.Path = "/" + uriEncodePath(key)
	default: // PathStyle
		u.Path = "/" + uriEncodePath(s.cfg.Bucket) + "/" + uriEncodePath(key)
	}
	return &u, u.Host, nil
}

// do signs and sends one request, with body already computed so its hash
// can be signed. extraHeaders are attached but not part of the signature
// (see signedRequest's doc).
func (s *Store) do(ctx context.Context, method, key string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	u, host, err := s.requestURL(key)
	if err != nil {
		return nil, err
	}

	sig := sign(s.cfg, s.now(), method, host, u.Path, body, extraHeaders)

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
		req.Header.Set(k, v)
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
