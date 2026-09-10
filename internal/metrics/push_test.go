package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPushIsAPUTOfTheExpositionToTheGroupingKeyPath(t *testing.T) {
	var gotMethod, gotPath, gotType, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
	}))
	defer srv.Close()

	err := Push(context.Background(), srv.URL,
		[]Label{{"job", "truss"}, {"pass", "drift"}}, "truss_x 1\n")
	if err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %s, want PUT -- POST leaves families this pass stopped emitting in place", gotMethod)
	}
	if want := "/metrics/job/truss/pass/drift"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if !strings.HasPrefix(gotType, "text/plain") {
		t.Errorf("Content-Type = %q, want the exposition format", gotType)
	}
	if gotBody != "truss_x 1\n" {
		t.Errorf("body = %q, want the exposition verbatim", gotBody)
	}
}

func TestATrailingSlashOnTheGatewayURLDoesNotDoubleTheSeparator(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer srv.Close()

	if err := Push(context.Background(), srv.URL+"/", []Label{{"job", "truss"}}, ""); err != nil {
		t.Fatalf("Push() error = %v", err)
	}
	if want := "/metrics/job/truss"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestANonSuccessStatusIsReportedWithItsCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "text format parsing error", http.StatusBadRequest)
	}))
	defer srv.Close()

	err := Push(context.Background(), srv.URL, []Label{{"job", "truss"}}, "bad\n")
	if err == nil {
		t.Fatal("Push() error = nil, want one -- a 400 discards the whole push")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("Push() error = %q, want it to name the status", err)
	}
}

// ⚠️ THE ONE TEST IN THIS FILE THAT IS ABOUT A SECRET. A gateway URL can
// carry credentials in its userinfo, and net/http embeds the request URL
// verbatim in the errors it returns -- so the natural implementation leaks
// the password into a log line on the first connection failure. deadman.Ping
// carries the same rule for the same reason and has its own copy of this
// test.
func TestNoErrorEverCarriesTheGatewayURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now: Do() fails and its error names the URL

	const secret = "hunter2"
	// The host is taken from the server rather than written out, so this
	// asserts against whatever address httptest chose -- and so that the
	// literal is not in this file at all, which is what scripts/leakscan
	// refuses on sight and is right to.
	host := strings.TrimPrefix(url, "http://")
	target := "http://user:" + secret + "@" + host

	err := Push(context.Background(), target, []Label{{"job", "truss"}}, "truss_x 1\n")
	if err == nil {
		t.Fatal("Push() error = nil, want a transport failure")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Push() error = %q, want the password nowhere in it", err)
	}
	if strings.Contains(err.Error(), host) {
		t.Errorf("Push() error = %q, want no part of the address in it", err)
	}
}

func TestPushRefusesAGroupingKeyItWouldHaveToEscape(t *testing.T) {
	for _, tc := range []struct {
		name  string
		base  string
		group []Label
		want  string
	}{
		{"no gateway", "", []Label{{"job", "truss"}}, "no gateway URL"},
		{"no grouping key", "http://gw", nil, "needs a grouping key"},
		{"job is not first", "http://gw", []Label{{"pass", "drift"}, {"job", "truss"}}, "must be job"},
		{"a slash in a value", "http://gw", []Label{{"job", "truss"}, {"pass", "a/b"}}, "cannot carry unescaped"},
		{"an empty value", "http://gw", []Label{{"job", "truss"}, {"pass", ""}}, "cannot carry unescaped"},
		{"an invalid label name", "http://gw", []Label{{"job", "truss"}, {"a-b", "v"}}, "not a valid grouping-key label name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Push(context.Background(), tc.base, tc.group, "")
			if err == nil {
				t.Fatalf("Push() error = nil, want one mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Push() error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
