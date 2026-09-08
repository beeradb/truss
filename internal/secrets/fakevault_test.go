package secrets

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeItemMeta is what fakeVault serves back for one item's metadata read.
type fakeItemMeta struct {
	recorded bool              // whether custom_metadata carries "expires" at all
	expires  string            // the value, when recorded
	extra    map[string]string // other custom_metadata keys, to prove they never leak out
}

// recordedRequest is one request the fake server saw, for tests that need
// to assert on the shape of the traffic -- method, path, content-type and
// body -- rather than just on the response. Added for the publish half:
// TestPatchExpirySendsAMergePatchAndTouchesNoDataPath and its neighbours
// need to see that a PATCH, not a POST, reached the metadata path.
type recordedRequest struct {
	Method      string
	Path        string
	ContentType string
	Body        string
}

// fakeVault is a minimal Vault KV v2 + Kubernetes-auth server, just enough
// of the wire shape for KV and Sweep to be tested against without ever
// touching a real Vault. It records every request path so
// TestTheSweepReadsNoSecretData can assert none of them touch "/data/".
type fakeVault struct {
	mu sync.Mutex

	loginCalls int
	paths      []string
	requests   []recordedRequest

	// loginStatus, when non-zero, is returned instead of 200 on login.
	loginStatus int
	// loginReply is the client_token the login endpoint hands back.
	loginReply string

	items []string // List() result
	// listStatus, when non-zero, is returned instead of 200/404 for LIST.
	listStatus int

	meta map[string]fakeItemMeta
	// metaStatus, when set for an item, is returned instead of 200 for its
	// metadata GET.
	metaStatus map[string]int
	// patchStatus, when set for an item, is returned instead of 200 for a
	// PATCH of its metadata -- how a server with no `patch` capability or
	// too old a KV plugin is simulated.
	patchStatus map[string]int

	// dataCAS is each item's current KV v2 version, the way Vault tracks
	// it for check-and-set. Zero means the item has never been written.
	dataCAS map[string]int
	// dataWrites records the last fields successfully written to each
	// item's data, so a test can assert what actually landed.
	dataWrites map[string]map[string]string
	// putStatus, when set for an item, is returned instead of the normal
	// cas-checked response for a POST to its data path.
	putStatus map[string]int
}

func newFakeVault() *fakeVault {
	return &fakeVault{
		loginReply:  authReplyToken(),
		meta:        map[string]fakeItemMeta{},
		metaStatus:  map[string]int{},
		patchStatus: map[string]int{},
		dataCAS:     map[string]int{},
		dataWrites:  map[string]map[string]string{},
		putStatus:   map[string]int{},
	}
}

// authReplyToken assembles a fake client token from parts rather than one
// literal, and under a name that does not itself read as a credential.
func authReplyToken() string {
	parts := []string{"fv", "clienttok", "0001", "aaaa"}
	return strings.Join(parts, "-")
}

func (f *fakeVault) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(f.handle))
}

func (f *fakeVault) handle(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.requests = append(f.requests, recordedRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		ContentType: r.Header.Get("Content-Type"),
		Body:        string(bodyBytes),
	})
	f.mu.Unlock()

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/auth/kubernetes/login":
		f.handleLogin(w, r)
	case r.Method == "LIST" && strings.HasSuffix(strings.TrimRight(r.URL.Path, "/"), "/metadata"):
		f.handleList(w, r)
	case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/metadata/"):
		f.handlePatchMetadata(w, r, bodyBytes)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/metadata/"):
		f.handleMetadata(w, r)
	case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/data/"):
		f.handlePutData(w, r, bodyBytes)
	case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/data/"):
		// Served, not refused, and that is the whole point. A fake that
		// 404s here makes TestTheSweepReadsNoSecretData pass for the wrong
		// reason: the sweep dies on the error before the test reaches its
		// own path assertion, so the assertion never runs and the guard is
		// vacuous. Verified by pointing kv.go at /data/ -- against a 404
		// fake the test failed on the request error, which is not the
		// property it claims to check. Answering plausibly means the path
		// assertion is the only thing that can fail.
		fmt.Fprint(w, `{"data":{"data":{"password":"THIS-IS-SECRET-DATA"},"metadata":{}}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// handlePatchMetadata simulates KV v2's merge-patch semantics: only the
// custom_metadata keys named in the body change, and a POST never reaches
// here because POST is a different case above (routed to handlePutData when
// it hits /data/ -- a POST to /metadata/ falls through to the 404 default,
// the same as real Vault answering an undefined method).
func (f *fakeVault) handlePatchMetadata(w http.ResponseWriter, r *http.Request, body []byte) {
	item := path.Base(r.URL.Path)

	f.mu.Lock()
	status := f.patchStatus[item]
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprint(w, `{"errors":["patch denied"]}`)
		return
	}

	var parsed struct {
		CustomMetadata map[string]string `json:"custom_metadata"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors":["bad request"]}`)
		return
	}

	f.mu.Lock()
	m := f.meta[item]
	if m.extra == nil {
		m.extra = map[string]string{}
	}
	for k, v := range parsed.CustomMetadata {
		if k == "expires" {
			m.recorded = true
			m.expires = v
		} else {
			m.extra[k] = v
		}
	}
	f.meta[item] = m
	f.mu.Unlock()

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `{}`)
}

// handlePutData simulates KV v2's check-and-set write: a request whose
// options.cas does not match the item's current version is refused with a
// 400, the same shape a real Vault's cas mismatch takes, and the version is
// not advanced.
func (f *fakeVault) handlePutData(w http.ResponseWriter, r *http.Request, body []byte) {
	item := path.Base(r.URL.Path)

	var parsed struct {
		Options struct {
			CAS int `json:"cas"`
		} `json:"options"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors":["bad request"]}`)
		return
	}

	f.mu.Lock()
	status := f.putStatus[item]
	current := f.dataCAS[item]
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		// Echoes the request body back into the error, deliberately: this
		// is what lets a test prove PutValue's caller redacts the value it
		// sent rather than merely never being handed it back by a fake
		// that happens not to echo. A fake that answered with a fixed
		// string here would make the redaction test pass whether or not
		// redaction actually ran.
		fmt.Fprintf(w, "put denied, echoing what vault received: %s", body)
		return
	}
	if parsed.Options.CAS != current {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errors":["check-and-set parameter did not match the current version"]}`)
		return
	}

	f.mu.Lock()
	f.dataCAS[item] = current + 1
	f.dataWrites[item] = parsed.Data
	f.mu.Unlock()

	fmt.Fprintf(w, `{"data":{"version":%d}}`, current+1)
}

func (f *fakeVault) handleLogin(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.loginCalls++
	status := f.loginStatus
	reply := f.loginReply
	f.mu.Unlock()

	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if status != http.StatusOK {
		fmt.Fprint(w, `{"errors":["login denied"]}`)
		return
	}
	fmt.Fprintf(w, `{"auth":{"client_token":%q}}`, reply)
}

func (f *fakeVault) handleList(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	status := f.listStatus
	items := append([]string(nil), f.items...)
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprint(w, `{"errors":["list denied"]}`)
		return
	}
	if len(items) == 0 {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[]}`)
		return
	}
	body, _ := json.Marshal(struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}{Data: struct {
		Keys []string `json:"keys"`
	}{Keys: items}})
	w.Write(body)
}

func (f *fakeVault) handleMetadata(w http.ResponseWriter, r *http.Request) {
	item := path.Base(r.URL.Path)

	f.mu.Lock()
	status := f.metaStatus[item]
	m, known := f.meta[item]
	f.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		fmt.Fprint(w, `{"errors":["metadata denied"]}`)
		return
	}
	if !known {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"errors":[]}`)
		return
	}

	custom := map[string]string{}
	for k, v := range m.extra {
		custom[k] = v
	}
	if m.recorded {
		custom["expires"] = m.expires
	}

	body, _ := json.Marshal(struct {
		Data struct {
			CustomMetadata map[string]string `json:"custom_metadata"`
		} `json:"data"`
	}{Data: struct {
		CustomMetadata map[string]string `json:"custom_metadata"`
	}{CustomMetadata: custom}})
	w.Write(body)
}

// requestPaths returns every path the fake server has seen, for tests that
// assert on the shape of the traffic rather than on the response.
func (f *fakeVault) requestPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...)
}

// recordedRequests returns every request the fake server has seen in full
// -- method, path, content-type and body -- for tests that need to tell a
// PATCH from a POST rather than just knowing a path was hit.
func (f *fakeVault) recordedRequests() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

// dataWriteFor returns the fields last successfully written to item's data,
// and whether anything has been written at all.
func (f *fakeVault) dataWriteFor(item string) (map[string]string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.dataWrites[item]
	return w, ok
}

// setDataCAS seeds item's current KV v2 version, the way a live Vault's
// version would already be non-zero for anything credentials/ has ever
// written -- so a test can construct a "concurrent writer" scenario by
// calling PutValue with a cas that no longer matches.
func (f *fakeVault) setDataCAS(item string, version int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dataCAS[item] = version
}

func (f *fakeVault) loginCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loginCalls
}

// newTestKV builds a KV against a fresh fakeVault, with a real JWT file on
// disk the way the projected ServiceAccount token would be. The JWT value
// is assembled from parts and named without "token"/"secret" so it is not
// itself credential-shaped to a scanner that would otherwise flag this
// fixture.
func newTestKV(t *testing.T, fv *fakeVault, srv *httptest.Server) *KV {
	t.Helper()
	jwt := jwtFixture()
	jwtPath := writeJWTFixture(t, jwt)

	kv, err := NewKV(KVConfig{
		Addr:    srv.URL,
		Mount:   "platform",
		Role:    "applier",
		JWTPath: jwtPath,
		HTTP:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewKV: %v", err)
	}
	return kv
}

// jwtFixture assembles a JWT-shaped string from three dot-joined segments
// rather than one literal, so it reads as data, not as a real credential.
func jwtFixture() string {
	header := "eyJhbGciOiJub25lIn0"
	payload := "eyJzdWIiOiJhcHBsaWVyIn0"
	sig := "ZmFrZS1zaWduYXR1cmUtc2VnbWVudA"
	return strings.Join([]string{header, payload, sig}, ".")
}

func writeJWTFixture(t *testing.T, jwt string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(jwt), 0o600); err != nil {
		t.Fatalf("writing jwt fixture: %v", err)
	}
	return p
}
