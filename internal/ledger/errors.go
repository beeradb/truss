package ledger

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
)

// ErrNotFound is returned by Get only when the key is absent -- a 404 or
// the XML NoSuchKey code. Never for a 403, a 5xx, a timeout, a DNS failure
// or a signature error: those are RequestError (or a wrapped transport
// error) so the caller can tell "does not exist" apart from "could not
// look" (§3.2).
var ErrNotFound = errors.New("ledger: key does not exist")

// RequestError is a request that reached the server and got an answer other
// than success or "not found". It names the HTTP status and, where the
// server sent one, the S3 error code and message -- so "the bucket refused
// the key" and "the bucket refused the credentials" produce different text
// rather than both reading as one flavour of failure.
type RequestError struct {
	Op         string // "get", "put" or "put-if-absent"
	Key        string
	StatusCode int
	Code       string // S3 <Error><Code>, when the body parsed as one
	Message    string // S3 <Error><Message>, when the body parsed as one
}

func (e *RequestError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("ledger: %s %q: %d %s: %s", e.Op, e.Key, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("ledger: %s %q: %d %s", e.Op, e.Key, e.StatusCode, http.StatusText(e.StatusCode))
}

// s3Error is the shape of the XML body S3-compatible APIs (including
// Google's) send on a non-2xx response.
type s3Error struct {
	XMLName xml.Name `xml:"Error"`
	Code    string   `xml:"Code"`
	Message string   `xml:"Message"`
}

// newRequestError builds a RequestError from a response, parsing the XML
// body when there is one. A body that does not parse as the expected shape
// leaves Code and Message empty rather than failing the whole call over a
// malformed error page.
func newRequestError(op, key string, statusCode int, body []byte) *RequestError {
	re := &RequestError{Op: op, Key: key, StatusCode: statusCode}
	var parsed s3Error
	if err := xml.Unmarshal(body, &parsed); err == nil {
		re.Code = parsed.Code
		re.Message = parsed.Message
	}
	return re
}
