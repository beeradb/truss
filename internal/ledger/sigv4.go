package ledger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// sigv4Service is the service name signed into the credential scope for any
// S3-compatible endpoint, including Google's.
const sigv4Service = "s3"

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// signingKey derives the SigV4 signing key for one day, region and service,
// per the AWS4-HMAC-SHA256 chain: kDate -> kRegion -> kService -> kSigning.
func signingKey(secret, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// signedRequest carries the headers store.go needs to attach to the
// *http.Request: Authorization, plus the two it must echo verbatim because
// they are inside the signature. Every header this client sends is signed
// -- see sign.
type signedRequest struct {
	amzDate       string // 20060102T150405Z
	payloadHash   string
	authorization string
}

// sign covers host, x-amz-content-sha256 and x-amz-date -- always -- plus
// every header in extra. Signing everything the client sends is deliberate
// and is the stronger of the two available rules: a header that reaches the
// server but sits outside the signature is a header an intermediary can add,
// drop or rewrite without invalidating the request, so the server may act on
// something the signature never vouched for. There is no code path here that
// sends an unsigned header.
//
// It is also load-bearing rather than merely tidy: an x-goog-* header left
// out of SignedHeaders is rejected with a bare 400, so anything this client
// ever adds has to be inside the signature to be usable at all.
//
// It does NOT buy create-if-absent, and an earlier version of this comment
// said it did. Measured in-cluster 2026-09-08: If-None-Match: * is accepted
// and then ignored (both writes 200, the second overwriting the first), and
// x-goog-if-generation-match: 0 is refused whether signed or not, with
// "ExcessHeaderValues: Requests cannot specify both x-amz and x-goog
// headers" -- and SigV4 forces the x-amz ones. There is no conditional-write
// primitive available to an S3-compatible client on this endpoint, which is
// why PutIfAbsent was deleted rather than fixed.
//
// Still deliberately absent, always: x-amz-checksum-*,
// x-amz-sdk-checksum-algorithm and x-amz-trailer (§4.2 "Checksums off unless
// required").
func sign(cfg Config, now time.Time, method, host, canonicalURI string, body []byte, extra map[string]string) signedRequest {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	payloadHash := hashHex(body)

	headers := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           amzDate,
	}
	for k, v := range extra {
		headers[strings.ToLower(k)] = v
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)

	var canonical strings.Builder
	for _, n := range names {
		// SigV4 canonicalises a header value by trimming it and collapsing
		// runs of spaces. Nothing here sends a value with either, but doing
		// it unconditionally means a caller adding one later cannot silently
		// produce a signature the server computes differently.
		canonical.WriteString(n)
		canonical.WriteByte(':')
		canonical.WriteString(collapseSpaces(headers[n]))
		canonical.WriteByte('\n')
	}
	canonicalHeaders := canonical.String()
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		"", // canonical query string: this package never sends one
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, cfg.Region, sigv4Service)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	key := signingKey(cfg.SecretAccessKey, dateStamp, cfg.Region, sigv4Service)
	signature := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))

	authorization := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		cfg.AccessKeyID, credentialScope, signedHeaders, signature,
	)

	return signedRequest{
		amzDate:       amzDate,
		payloadHash:   payloadHash,
		authorization: authorization,
	}
}

// uriEncodePath percent-encodes each path segment per SigV4's rules
// (unreserved: A-Za-z0-9-_.~) while leaving "/" as a literal separator.
// Object keys are already path segments -- a key containing "/" names a
// hierarchy, not a character to escape -- and this is the S3 exception to
// SigV4's general double-encoding rule: encode once, never twice.
func uriEncodePath(p string) string {
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		segments[i] = uriEncodeSegment(seg)
	}
	return strings.Join(segments, "/")
}

func uriEncodeSegment(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if isUnreserved(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func isUnreserved(c byte) bool {
	switch {
	case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9':
		return true
	case c == '-' || c == '_' || c == '.' || c == '~':
		return true
	default:
		return false
	}
}

// collapseSpaces applies SigV4's header-value normalisation: leading and
// trailing whitespace removed, and any internal run of spaces collapsed to
// one. A server that normalises and a client that does not disagree on the
// signature, and the error it produces names the key rather than the space.
func collapseSpaces(v string) string {
	return strings.Join(strings.Fields(v), " ")
}
