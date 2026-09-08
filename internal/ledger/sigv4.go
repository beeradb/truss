package ledger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// signedRequest carries exactly the headers store.go needs to attach to the
// *http.Request: the three that are part of the signature (Host,
// X-Amz-Content-Sha256, X-Amz-Date) plus Authorization. Anything sent that
// is not part of the signature -- If-None-Match on PutIfAbsent, say -- is
// the caller's to add.
type signedRequest struct {
	amzDate       string // 20060102T150405Z
	payloadHash   string
	authorization string
}

// sign builds the exact three signed headers measured against the real
// bucket on 2026-09-08 -- host, x-amz-content-sha256, x-amz-date, in that
// order, which is also their sort order so no special-casing is needed to
// produce SignedHeaders. No other header is folded into the signature:
// nothing here adds x-amz-checksum-*, x-amz-sdk-checksum-algorithm or an
// x-amz-trailer, which is the whole point (§4.2 "Checksums off unless
// required").
func sign(cfg Config, now time.Time, method, host, canonicalURI string, body []byte) signedRequest {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	payloadHash := hashHex(body)

	canonicalHeaders := fmt.Sprintf(
		"host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		host, payloadHash, amzDate,
	)
	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"

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
