package deadman

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPingHitsTheServer proves a successful ping actually reaches the
// monitor -- a check nobody has watched fail is a claim, and a test that
// only checked "no error" could pass against a Ping that never sent anything.
func TestPingHitsTheServer(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := Ping(context.Background(), srv.URL); err != nil {
		t.Fatalf("Ping returned an error for a 200 response: %v", err)
	}
	if !hit {
		t.Fatalf("the fake monitor was never hit")
	}
}

// TestPingReportsANon2xxStatusWithoutTheURL checks both that a 500 is
// reported as an error and that the error text never carries the server's
// URL -- the ping URL is the bearer secret this whole feature exists to
// protect, so its text must never leak into anything logged.
func TestPingReportsANon2xxStatusWithoutTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := Ping(context.Background(), srv.URL)
	if err == nil {
		t.Fatalf("Ping returned no error for a 500 response")
	}
	if strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("Ping's error leaks the monitor URL: %q", err.Error())
	}
}

// TestPingReportsAClosedServerWithoutTheURL is the "server is closed"
// case: a dial failure, which net/http's own error type would otherwise
// spell the request URL into.
func TestPingReportsAClosedServerWithoutTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	err := Ping(context.Background(), url)
	if err == nil {
		t.Fatalf("Ping returned no error for a closed server")
	}
	if strings.Contains(err.Error(), url) {
		t.Fatalf("Ping's error leaks the monitor URL: %q", err.Error())
	}
}
