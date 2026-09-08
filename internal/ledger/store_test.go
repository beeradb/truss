package ledger

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newStoreWithHandler(t *testing.T, handler http.HandlerFunc) (*Store, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	cfg := Config{
		Endpoint:        srv.URL,
		Bucket:          "a-bucket",
		Region:          "us-east-1",
		AccessKeyID:     "AKIAFAKE",
		SecretAccessKey: "fakesecretfakesecretfakesecret",
	}
	store, err := New(cfg, WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store, srv
}

// TestGetReturnsErrNotFoundOnlyForAbsence: §3.2. A reader that reports one
// failure for a 404, a 403, a 500 and a DNS failure alike makes "nobody has
// written this yet" indistinguishable from "the bucket is unreachable", and
// the applier's refusals turn on exactly that difference. Get must
// distinguish "does not exist" from every other way a request can fail to
// come back with a value.
func TestGetReturnsErrNotFoundOnlyForAbsence(t *testing.T) {
	t.Run("404 is ErrNotFound", func(t *testing.T) {
		store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
			xmlError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		})
		defer srv.Close()

		_, err := store.Get(context.Background(), "applier/head")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Get on a 404 = %v, want errors.Is(err, ErrNotFound)", err)
		}
	})

	t.Run("403 is not ErrNotFound and names itself", func(t *testing.T) {
		store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
			xmlError(w, http.StatusForbidden, "AccessDenied", "Access Denied")
		})
		defer srv.Close()

		_, err := store.Get(context.Background(), "applier/head")
		if err == nil {
			t.Fatal("Get on a 403 = nil error")
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("Get on a 403 was reported as ErrNotFound: %v", err)
		}
		if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "AccessDenied") {
			t.Errorf("error does not name what happened: %v", err)
		}
	})

	t.Run("signature error is not ErrNotFound and names itself", func(t *testing.T) {
		store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
			xmlError(w, http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.")
		})
		defer srv.Close()

		_, err := store.Get(context.Background(), "applier/head")
		if errors.Is(err, ErrNotFound) {
			t.Errorf("Get on a signature error was reported as ErrNotFound: %v", err)
		}
		if !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
			t.Errorf("error does not name the signature failure: %v", err)
		}
	})

	t.Run("500 is not ErrNotFound", func(t *testing.T) {
		store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
			xmlError(w, http.StatusInternalServerError, "InternalError", "We encountered an internal error.")
		})
		defer srv.Close()

		_, err := store.Get(context.Background(), "applier/head")
		if errors.Is(err, ErrNotFound) {
			t.Errorf("Get on a 500 was reported as ErrNotFound: %v", err)
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error does not name the status: %v", err)
		}
	})

	t.Run("a connection failure is not ErrNotFound", func(t *testing.T) {
		// A closed listener stands in for "could not reach the endpoint at
		// all" -- a DNS failure and a refused connection both surface to
		// Go as a transport-level error from http.Client.Do, before any
		// status code exists to classify.
		store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {})
		srv.Close()

		_, err := store.Get(context.Background(), "applier/head")
		if err == nil {
			t.Fatal("Get against a closed listener = nil error")
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("Get against a closed listener was reported as ErrNotFound: %v", err)
		}
	})
}

// TestGetOfAMissingKeyIsNeverAnEmptySuccess: Get must never return a nil
// error with an empty (or any) body for a key that is not there.
func TestGetOfAMissingKeyIsNeverAnEmptySuccess(t *testing.T) {
	store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		xmlError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
	})
	defer srv.Close()

	body, err := store.Get(context.Background(), "applier/head")
	if err == nil {
		t.Fatalf("Get on a missing key returned a nil error, body=%q", body)
	}
	if body != nil {
		t.Errorf("Get on a missing key returned a non-nil body: %q", body)
	}
}

// TestPutSendsNoChecksumTrailer asserts the wire, not a config field
// (§5.3): first that a Put through this Store never carries the headers
// that make Google's XML API answer SignatureDoesNotMatch / Invalid
// argument, and second -- as a check on the test double itself -- that the
// fake server actually rejects a request that does carry one, so a
// checksum regression here could not pass by accident.
func TestPutSendsNoChecksumTrailer(t *testing.T) {
	store, fb, srv := newFakeServerStore(t, PathStyle)
	defer srv.Close()

	if err := store.Put(context.Background(), "applier/head", []byte("headsha1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	forbidden := []string{"X-Amz-Trailer", "X-Amz-Sdk-Checksum-Algorithm", "X-Amz-Checksum-Crc32", "X-Amz-Checksum-Sha256"}
	for _, h := range forbidden {
		if v := fb.lastReq.Header.Get(h); v != "" {
			t.Errorf("Put sent forbidden header %s: %s", h, v)
		}
	}
	if fb.lastReq.Header.Get("Content-Encoding") == "aws-chunked" {
		t.Errorf("Put sent Content-Encoding: aws-chunked")
	}

	// Sanity-check the fixture: a raw request carrying one of the forbidden
	// headers must be rejected, proving the server enforces what this test
	// relies on rather than accepting everything.
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/ledger-test-bucket/probe-key", bytes.NewReader([]byte("x")))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("raw checksum-trailer request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fake server did not reject a checksum trailer: status %d (the fixture is not testing what this test needs it to test)", resp.StatusCode)
	}
}

// TestPutIsExactBytes: no re-encoding, no added newline, no re-framing --
// what Put is given is what lands in the object, including an empty body,
// an embedded NUL, and bytes that are not valid UTF-8.
func TestPutIsExactBytes(t *testing.T) {
	store, fb, srv := newFakeServerStore(t, PathStyle)
	defer srv.Close()

	cases := map[string][]byte{
		"empty":       {},
		"with-nul":    []byte("before\x00after"),
		"non-utf8":    {0xff, 0xfe, 0x00, 0x80, 0x81},
		"json-ish":    []byte(`{"reason":"boom","at":"2026-09-08T00:00:00Z"}`),
		"trailing-ws": []byte("keep this trailing space \n"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			key := "applier/exact/" + name
			if err := store.Put(context.Background(), key, body); err != nil {
				t.Fatalf("Put: %v", err)
			}
			fb.mu.Lock()
			stored, ok := fb.objects[key]
			fb.mu.Unlock()
			if !ok {
				t.Fatalf("object was not stored")
			}
			if !bytes.Equal(stored, body) {
				t.Errorf("stored bytes = %v, want %v", stored, body)
			}
		})
	}
}

// TestReadStripsATrailingNewline: Get strips trailing whitespace, because
// every value in the ledger is a single token and a newline left on the end
// of one fails the comparison it was read for.
func TestReadStripsATrailingNewline(t *testing.T) {
	store, _, srv := newFakeServerStore(t, PathStyle)
	defer srv.Close()
	ctx := context.Background()

	cases := []struct {
		name  string
		write string
		want  string
	}{
		{"single newline", "headsha1\n", "headsha1"},
		{"multiple newlines", "headsha1\n\n\n", "headsha1"},
		{"crlf", "headsha1\r\n", "headsha1"},
		{"no trailing whitespace", "headsha1", "headsha1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := "applier/head/" + tc.name
			if err := store.Put(ctx, key, []byte(tc.write)); err != nil {
				t.Fatalf("Put: %v", err)
			}
			got, err := store.Get(ctx, key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Get(%q) = %q, want %q", tc.write, got, tc.want)
			}
		})
	}
}

// TestStoreHasNoDeleteOperation: §4.2 "Refuses to. ... Expose a Delete."
// Nothing in this project ever removes a ledger entry; the guarantee is
// that the method does not exist, checked here so a future edit adding one
// fails a test rather than silently making removal possible.
func TestStoreHasNoDeleteOperation(t *testing.T) {
	typ := reflect.TypeOf(&Store{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if strings.Contains(strings.ToLower(name), "delete") || strings.Contains(strings.ToLower(name), "remove") {
			t.Errorf("Store exposes %s, which §4.2 refuses", name)
		}
	}
}

// TestAddressingStyleControlsTheRequestURL confirms the two addressing
// styles actually produce different requests -- PathStyle folds the bucket
// into the path, VirtualHostStyle folds it into the Host -- since Config
// makes this an explicit choice rather than a guessed default (§8).
func TestAddressingStyleControlsTheRequestURL(t *testing.T) {
	t.Run("path style", func(t *testing.T) {
		store, fb, srv := newFakeServerStore(t, PathStyle)
		defer srv.Close()
		if err := store.Put(context.Background(), "applier/head", []byte("headsha1")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if !strings.HasPrefix(fb.lastReq.URL.Path, "/ledger-test-bucket/") {
			t.Errorf("path-style request path = %s, want the bucket as the first segment", fb.lastReq.URL.Path)
		}
	})

	t.Run("virtual-host style", func(t *testing.T) {
		store, fb, srv := newFakeServerStore(t, VirtualHostStyle)
		defer srv.Close()
		if err := store.Put(context.Background(), "applier/head", []byte("headsha1")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if !strings.HasPrefix(fb.lastReq.Host, "ledger-test-bucket.") {
			t.Errorf("virtual-host request Host = %s, want it prefixed with the bucket name", fb.lastReq.Host)
		}
		if fb.lastReq.URL.Path != "/applier/head" {
			t.Errorf("virtual-host request path = %s, want /applier/head with no bucket segment", fb.lastReq.URL.Path)
		}
	})
}

// TestTheSignedPathIsThePathOnTheWire covers a bug that shipped and that no
// test could see. requestURL assigned its own percent-encoding to
// url.URL.Path, but Path holds the DECODED path and URL.String() escapes it
// again -- so a key containing a space was signed as %20 and sent as %2520,
// and the signature covered a path the server never received. It failed
// closed (SignatureDoesNotMatch, never a wrong object) and it was invisible
// because every key in production today is unreserved characters only. Root
// names reach DigestKey, so a root directory named with a space is all it
// would have taken.
//
// The assertion is deliberately end-to-end rather than a property of
// requestURL: it re-signs the path the SERVER received and requires that to
// reproduce the Authorization header the client sent. A test that only
// checked requestURL for internal consistency passed with the bug in place.
func TestTheSignedPathIsThePathOnTheWire(t *testing.T) {
	fixed := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	for _, key := range []string{
		"applier/plans/abc123/credentials.digest", // the ordinary case
		"applier/plans/abc/root with space.digest",
		"applier/plans/abc/a+b.digest",
		"applier/plans/abc/tilde~ok.digest",
	} {
		t.Run(key, func(t *testing.T) {
			var gotPath, gotAuth, gotHost string
			store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.EscapedPath()
				gotAuth = r.Header.Get("Authorization")
				gotHost = r.Host
				w.WriteHeader(http.StatusOK)
			})
			defer srv.Close()
			store.now = func() time.Time { return fixed }

			body := []byte(`{"digest":"deadbeef"}`)
			if err := store.Put(context.Background(), key, body); err != nil {
				t.Fatalf("Put: %v", err)
			}
			if gotAuth == "" {
				t.Fatal("no Authorization header reached the server")
			}

			want := sign(store.cfg, fixed, http.MethodPut, gotHost, gotPath, body, nil).authorization
			if gotAuth != want {
				t.Errorf("the signature does not cover the path the server received (%s)\n sent %s\nover-received-path %s",
					gotPath, gotAuth, want)
			}
		})
	}
}

// knownUnsignedTransportHeaders are the headers net/http.Transport adds
// after sign() has run, so they cannot be inside SignedHeaders. They are
// listed EXACTLY rather than matched by shape, so that a new unsigned header
// -- one this package starts setting outside the signature, or a future Go
// release starts adding -- fails this test instead of widening silently.
//
// None is exploitable: the signed body hash bounds Content-Length, and the
// other two are inert. The point of pinning them is that the invariant is
// now checked against the wire rather than asserted in a comment.
var knownUnsignedTransportHeaders = map[string]bool{
	"Accept-Encoding": true,
	"User-Agent":      true,
	"Content-Length":  true,
}

// TestOnlyKnownTransportHeadersTravelUnsigned replaces a claim with a
// measurement. sigv4.go used to say "there is no code path here that sends an
// unsigned header", and the test named for it never sent a request -- it
// inspected sign()'s own return value. Found by the 2026-09-08 code audit and
// security review, independently.
func TestOnlyKnownTransportHeadersTravelUnsigned(t *testing.T) {
	var got http.Header
	store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		got.Set("Host", r.Host) // Host is not in r.Header, but it is signed
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	if err := store.Put(context.Background(), "applier/head", []byte("abc")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no headers reached the server, so this test could not have failed")
	}

	signed := map[string]bool{}
	for _, n := range signedHeaderList(t, got.Get("Authorization")) {
		signed[n] = true
	}
	if len(signed) == 0 {
		t.Fatal("no SignedHeaders on the request")
	}

	for name := range got {
		if name == "Authorization" {
			continue // carries the signature; cannot be inside it
		}
		if signed[strings.ToLower(name)] {
			continue
		}
		if knownUnsignedTransportHeaders[name] {
			continue
		}
		t.Errorf("header %q is on the wire but is not signed and is not a known transport header", name)
	}
}

// TestTheHeaderValueSignedIsTheHeaderValueSent is the value-level counterpart
// of TestTheSignedPathIsThePathOnTheWire. sign canonicalises a header value
// (trim, collapse internal runs of spaces) before hashing it, per SigV4; do()
// used to send the RAW value, so the two differed for any padded value. Fail-
// closed and latent -- no caller passes a padded value today -- but the same
// signed-bytes-versus-sent-bytes class as the Path/RawPath bug.
//
// ⚠️ THE OBVIOUS ASSERTION HERE IS VACUOUS AND WAS WRITTEN FIRST. Re-signing
// the value the server received cannot detect this bug: sign() collapses
// whatever it is handed, so the collapse is idempotent and the re-signed
// header reproduces the sent one either way. Verified by mutation -- sending
// the raw value left that version green. The assertion has to compare the
// received value against the COLLAPSED form directly, because that is the
// byte sequence the signature actually covers.
func TestTheHeaderValueSignedIsTheHeaderValueSent(t *testing.T) {
	const padded = "  application/json;   charset=utf-8  "

	var gotValue string
	store, srv := newStoreWithHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotValue = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	if _, err := store.do(context.Background(), http.MethodPut, "applier/head",
		[]byte("abc"), map[string]string{"content-type": padded}); err != nil {
		t.Fatalf("do: %v", err)
	}

	want := collapseSpaces(padded)
	if want == padded {
		t.Fatal("the fixture value needs no collapsing, so this test could not have failed")
	}
	if gotValue != want {
		t.Errorf("the value on the wire is not the value signed\n wire %q\nsigned %q", gotValue, want)
	}
}
