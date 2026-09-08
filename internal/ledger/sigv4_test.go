package ledger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// The signer had NO offline test until 2026-09-08, and the gap was found by
// mutation rather than by reading: pinning the region to a constant, and
// truncating SignedHeaders to its first entry, both left the whole package
// green. The only thing verifying the signature was TestAgainstTheRealBucket,
// which skips unless the in-cluster bucket credentials are set -- so CI could
// not have caught a signing regression at all.
//
// These are DIFFERENTIAL tests against referenceSign below, a second
// AWS4-HMAC-SHA256 implementation written from the specification's step list
// and deliberately structured differently from sigv4.go. Two implementations
// that disagree localise the bug; two that agree over thousands of randomised
// inputs are unlikely to share an accident.
//
// ⚠️ Differential testing cannot catch a misreading BOTH implementations
// share. The external anchor for that is TestAgainstTheRealBucket, which puts
// real signatures in front of the real endpoint and additionally checks
// bidirectional parity with the AWS CLI. What lives here is the regression
// lock that runs in CI with no credentials.
//
// ⚠️ Golden hex strings were tried first and removed: scripts/leakscan
// refuses a 32+ character hex literal, and it is right to -- it cannot tell a
// SigV4 test vector from an account id, and the fix for that is not to teach
// it an allowlist that anything could then use.

// referenceSign computes the Authorization header per the AWS4-HMAC-SHA256
// step list, independently of sign(). It takes headers already assembled so
// that the two implementations agree on WHAT is being signed while remaining
// free to disagree on HOW.
func referenceSign(accessKeyID, secret, region, service string, t time.Time,
	method, canonicalURI string, headers map[string]string, payloadHash string) string {

	// Step 1: canonical request.
	var names []string
	for k := range headers {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)

	lowered := map[string]string{}
	for k, v := range headers {
		lowered[strings.ToLower(k)] = strings.Join(strings.Fields(v), " ")
	}

	lines := []string{method, canonicalURI, ""}
	block := ""
	for _, n := range names {
		block += n + ":" + lowered[n] + "\n"
	}
	signedHeaders := strings.Join(names, ";")
	lines = append(lines, block, signedHeaders, payloadHash)
	canonicalRequest := strings.Join(lines, "\n")

	// Step 2: string to sign.
	stamp := t.UTC().Format("20060102")
	amzDate := t.UTC().Format("20060102T150405Z")
	scope := strings.Join([]string{stamp, region, service, "aws4_request"}, "/")
	crHash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, hex.EncodeToString(crHash[:]),
	}, "\n")

	// Step 3: signing key, as a fold over the four scope components.
	key := []byte("AWS4" + secret)
	for _, part := range []string{stamp, region, service, "aws4_request"} {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(part))
		key = m.Sum(nil)
	}

	// Step 4: signature.
	m := hmac.New(sha256.New, key)
	m.Write([]byte(stringToSign))
	signature := hex.EncodeToString(m.Sum(nil))

	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKeyID, scope, signedHeaders, signature)
}

// testCfg uses AWS's own documented example key id and secret: they are
// published placeholders, identical for every reader of the specification,
// and identify no deployment. The region is deliberately NOT us-east-1 (the
// package default), so a signer that ignores cfg.Region and hardcodes the
// default fails rather than passing by accident.
func testCfg() Config {
	return Config{
		Endpoint:        "https://storage.googleapis.com",
		Bucket:          "a-bucket",
		Region:          "eu-west-1",
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
}

// expected mirrors what sign() sends, so referenceSign is given the same
// header set: host, the two x-amz headers sign always adds, plus extras.
func expected(cfg Config, now time.Time, method, host, uri string, body []byte, extra map[string]string) string {
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           now.UTC().Format("20060102T150405Z"),
	}
	for k, v := range extra {
		headers[strings.ToLower(k)] = v
	}
	return referenceSign(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.Region, "s3",
		now, method, uri, headers, payloadHash)
}

func TestSignAgreesWithAnIndependentImplementation(t *testing.T) {
	cfg := testCfg()
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	const host = "storage.googleapis.com"

	cases := []struct {
		name        string
		method, uri string
		body        []byte
		extra       map[string]string
	}{
		{"put with a body", "PUT", "/a-bucket/applier/plans/abc/credentials.digest",
			[]byte(`{"digest":"deadbeef"}`), nil},
		{"get with no body", "GET", "/a-bucket/applier/head", nil, nil},
		{"one extra header", "PUT", "/a-bucket/applier/head",
			[]byte("abc"), map[string]string{"content-type": "application/json"}},
		{"several extra headers", "PUT", "/a-bucket/applier/head", []byte("abc"),
			map[string]string{"content-type": "application/json", "x-amz-storage": "STANDARD"}},
		{"body with a NUL byte", "PUT", "/a-bucket/applier/head",
			[]byte{'a', 0, 'b'}, nil},
		{"percent-encoded path", "PUT", "/a-bucket/applier/plans/abc/root%20with%20space.digest",
			[]byte("abc"), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sign(cfg, now, c.method, host, c.uri, c.body, c.extra).authorization
			want := expected(cfg, now, c.method, host, c.uri, c.body, c.extra)
			if got != want {
				t.Errorf("authorization\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestSignAgreesOverRandomisedInputs is the wide version of the above. A
// fixed vector proves one point; a mutation that only misbehaves on, say, a
// header name that sorts oddly or a region of a particular length survives
// four cases and not two thousand.
func TestSignAgreesOverRandomisedInputs(t *testing.T) {
	cfg := testCfg()
	const host = "storage.googleapis.com"
	rng := rand.New(rand.NewSource(20260908)) // fixed seed: reproducible failures

	methods := []string{"GET", "PUT", "HEAD"}
	regions := []string{"eu-west-1", "us-east-1", "us-central1", "auto", "a"}
	headerPool := []string{"content-type", "cache-control", "x-amz-storage",
		"x-amz-meta-root", "if-match", "z-last", "a-first"}

	for i := 0; i < 2000; i++ {
		c := cfg
		c.Region = regions[rng.Intn(len(regions))]
		now := time.Unix(rng.Int63n(2_000_000_000), 0).UTC()
		method := methods[rng.Intn(len(methods))]

		body := make([]byte, rng.Intn(64))
		rng.Read(body)
		if rng.Intn(4) == 0 {
			body = nil
		}

		uri := fmt.Sprintf("/a-bucket/applier/plans/%x/root-%d.digest", rng.Int63(), rng.Intn(99))

		extra := map[string]string{}
		for _, h := range headerPool {
			if rng.Intn(3) == 0 {
				extra[h] = fmt.Sprintf("v%d", rng.Intn(1000))
			}
		}
		if len(extra) == 0 {
			extra = nil
		}

		got := sign(c, now, method, host, uri, body, extra).authorization
		want := expected(c, now, method, host, uri, body, extra)
		if got != want {
			t.Fatalf("iteration %d disagreed\nregion=%s method=%s uri=%s extra=%v\n got %s\nwant %s",
				i, c.Region, method, uri, extra, got, want)
		}
	}
}

// TestSigningKeyIsDerivedFromTheConfiguredRegion is the direct answer to
// mutation M1: kRegion built from a hardcoded "us-east-1" rather than from
// cfg.Region. Stated as a property so no literal key material is needed.
func TestSigningKeyIsDerivedFromTheConfiguredRegion(t *testing.T) {
	secret := testCfg().SecretAccessKey
	a := signingKey(secret, "20150830", "eu-west-1", "s3")
	b := signingKey(secret, "20150830", "us-east-1", "s3")
	if hmac.Equal(a, b) {
		t.Fatal("the signing key does not depend on the region")
	}
	for _, vary := range []struct {
		name                   string
		stamp, region, service string
	}{
		{"date", "20150831", "eu-west-1", "s3"},
		{"region", "20150830", "eu-west-2", "s3"},
		{"service", "20150830", "eu-west-1", "s4"},
	} {
		if hmac.Equal(a, signingKey(secret, vary.stamp, vary.region, vary.service)) {
			t.Errorf("the signing key does not depend on the %s", vary.name)
		}
	}
	if hmac.Equal(a, signingKey(secret+"x", "20150830", "eu-west-1", "s3")) {
		t.Error("the signing key does not depend on the secret")
	}
}

// TestEveryHeaderSentIsSigned is the direct answer to mutation M2: building
// SignedHeaders from a subset of the headers actually sent. A header the
// server honours but the signature does not cover is one an intermediary can
// add, drop or rewrite without invalidating the request.
func TestEveryHeaderSentIsSigned(t *testing.T) {
	cfg := testCfg()
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	extra := map[string]string{
		"content-type":  "application/json",
		"x-amz-storage": "STANDARD",
	}
	got := sign(cfg, now, "PUT", "storage.googleapis.com", "/a-bucket/k", []byte("abc"), extra)

	signed := signedHeaderList(t, got.authorization)
	for _, want := range []string{
		"content-type", "host", "x-amz-content-sha256", "x-amz-date", "x-amz-storage",
	} {
		if !contains(signed, want) {
			t.Errorf("header %q is sent but is not in SignedHeaders=%v", want, signed)
		}
	}
	if len(signed) != 5 {
		t.Errorf("SignedHeaders has %d entries, want exactly the 5 sent: %v", len(signed), signed)
	}
	for i := 1; i < len(signed); i++ {
		if signed[i-1] >= signed[i] {
			t.Fatalf("SignedHeaders is not sorted ascending: %v", signed)
		}
	}
}

// TestAnExtraHeaderChangesTheSignature stops the test above passing against a
// signer that lists a header in SignedHeaders without folding it into the
// canonical request -- a signature over a document claiming to cover a header
// it does not.
func TestAnExtraHeaderChangesTheSignature(t *testing.T) {
	cfg := testCfg()
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	with := sign(cfg, now, "PUT", "h", "/a-bucket/k", []byte("abc"),
		map[string]string{"content-type": "application/json"}).authorization
	without := sign(cfg, now, "PUT", "h", "/a-bucket/k", []byte("abc"), nil).authorization
	if with == without {
		t.Fatal("adding a signed header left the signature unchanged")
	}
}

// TestHeaderValuesAreCollapsedBeforeSigning: SigV4 requires the signer to trim
// a header value and collapse internal runs of spaces, because the server
// does. A client that skips it produces a signature the server computes
// differently, and the error names the credential rather than the space.
func TestHeaderValuesAreCollapsedBeforeSigning(t *testing.T) {
	cfg := testCfg()
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	padded := sign(cfg, now, "PUT", "h", "/a-bucket/k", []byte("abc"),
		map[string]string{"content-type": "  application/json;   charset=utf-8  "}).authorization
	tidy := sign(cfg, now, "PUT", "h", "/a-bucket/k", []byte("abc"),
		map[string]string{"content-type": "application/json; charset=utf-8"}).authorization
	if padded != tidy {
		t.Fatalf("padded and collapsed header values signed differently\npadded %s\n tidy %s", padded, tidy)
	}
}

func TestHeaderNamesAreLowercasedBeforeSigning(t *testing.T) {
	cfg := testCfg()
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	mixed := sign(cfg, now, "PUT", "h", "/a-bucket/k", []byte("abc"),
		map[string]string{"Content-Type": "application/json"}).authorization
	lower := sign(cfg, now, "PUT", "h", "/a-bucket/k", []byte("abc"),
		map[string]string{"content-type": "application/json"}).authorization
	if mixed != lower {
		t.Fatalf("header name case changed the signature\nmixed %s\nlower %s", mixed, lower)
	}
}

// TestTheSecretNeverAppearsInASignedRequest guards the audit item "the secret
// or the derived signing key reaching an error, a log, or a header dump".
// signedRequest is the only thing that leaves sigv4.go.
func TestTheSecretNeverAppearsInASignedRequest(t *testing.T) {
	cfg := testCfg()
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	got := sign(cfg, now, "PUT", "h", "/a-bucket/k", []byte("abc"),
		map[string]string{"content-type": "application/json"})

	key := hex.EncodeToString(signingKey(cfg.SecretAccessKey, "20150830", cfg.Region, "s3"))
	for _, field := range []string{got.authorization, got.amzDate, got.payloadHash} {
		if strings.Contains(field, cfg.SecretAccessKey) {
			t.Error("the secret access key appears in a signed request field")
		}
		if strings.Contains(field, key) {
			t.Error("the derived signing key appears in a signed request field")
		}
	}
	// The access key id is public and IS expected in Credential=.
	if !strings.Contains(got.authorization, cfg.AccessKeyID) {
		t.Error("the access key id should be in Credential=; the fixture may be wrong")
	}
}

// TestTheBodyIsAlwaysHashed guards the audit item "a fallback to an unsigned
// or UNSIGNED-PAYLOAD request on some path". SigV4 permits a client to sign
// the literal string UNSIGNED-PAYLOAD in place of the body hash; this package
// must never do that, because the body of a ledger write is the record the
// whole plan-digest gate rests on.
func TestTheBodyIsAlwaysHashed(t *testing.T) {
	cfg := testCfg()
	now := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	for _, body := range [][]byte{nil, {}, []byte("abc"), {0, 1, 2}} {
		got := sign(cfg, now, "PUT", "h", "/a-bucket/k", body, nil)
		sum := sha256.Sum256(body)
		if got.payloadHash != hex.EncodeToString(sum[:]) {
			t.Errorf("body %q: payload hash is not the SHA-256 of the body: %s", body, got.payloadHash)
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("could not read package sources: %v", err)
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		scanned++
		if strings.Contains(string(src), "UNSIGNED-PAYLOAD") {
			t.Errorf("%s signs UNSIGNED-PAYLOAD in place of the body hash", f)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files, so this check could not have failed")
	}
}

func signedHeaderList(t *testing.T, authorization string) []string {
	t.Helper()
	const marker = "SignedHeaders="
	i := strings.Index(authorization, marker)
	if i < 0 {
		t.Fatalf("no SignedHeaders in %q", authorization)
	}
	rest := authorization[i+len(marker):]
	if j := strings.Index(rest, ","); j >= 0 {
		rest = rest[:j]
	}
	return strings.Split(rest, ";")
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
