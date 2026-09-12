package tailnet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient builds a Client pointed at srv, the way every test here
// needs one -- BaseURL overridden is the whole reason Config carries the
// field (see its doc comment).
func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Config{
		APIKey:  "tskey-api-" + "test" + "00112233",
		Tailnet: "example.invalid",
		BaseURL: baseURL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestDevicesDecodesAWellFormedResponse exercises the verified shape: the
// endpoint path, the "devices" wrapper key, and the name/tags/lastSeen
// field spellings from the OpenAPI document cited in client.go.
//
// ⚠️ THE NAMES ARE MagicDNS NAMES BECAUSE THAT IS WHAT THE API SENDS. This
// fixture used bare names ("web-1") for its whole life, while the schema's
// own example is "pangolin.tailfe8c.ts.net" -- so the suite agreed with
// Reconcile's bare inventory names and production never could.
func TestDevicesDecodesAWellFormedResponse(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"devices": [
				{
					"name": "web-1.tailfe8c.ts.net",
					"tags": ["tag:k8s"],
					"lastSeen": "2026-09-01T12:00:00Z",
					"connectedToControl": false
				},
				{
					"name": "web-2.tailfe8c.ts.net",
					"tags": [],
					"connectedToControl": true
				}
			]
		}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	devices, err := c.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(devices))
	}

	if devices[0].Name != "web-1" {
		t.Errorf("devices[0].Name = %q, want %q", devices[0].Name, "web-1")
	}
	if len(devices[0].Tags) != 1 || devices[0].Tags[0] != "tag:k8s" {
		t.Errorf("devices[0].Tags = %v, want [tag:k8s]", devices[0].Tags)
	}
	want, _ := time.Parse(time.RFC3339, "2026-09-01T12:00:00Z")
	if !devices[0].LastSeen.Equal(want) {
		t.Errorf("devices[0].LastSeen = %v, want %v", devices[0].LastSeen, want)
	}

	// web-2 omits lastSeen and is connected right now: LastSeen must not
	// be the zero value, which would read as "never seen" and make a
	// live device look maximally stale to Reconcile.
	if devices[1].LastSeen.IsZero() {
		t.Errorf("devices[1].LastSeen is zero for a device connected right now")
	}

	wantPath := "/api/v2/tailnet/example.invalid/devices"
	if gotPath != wantPath {
		t.Errorf("request path = %q, want %q", gotPath, wantPath)
	}
	if gotAuth != "Bearer tskey-api-test00112233" {
		t.Errorf("Authorization header = %q, want a Bearer token", gotAuth)
	}
}

// TestDevicesOnEmptyTailnetIsAnEmptySliceNotAnError proves the distinction
// internal/secrets.Store.List's own comment makes: a tailnet that genuinely
// has no devices must decode as (non-nil empty slice, nil error), never be
// confused with a read that failed.
func TestDevicesOnEmptyTailnetIsAnEmptySliceNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"devices": []}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	devices, err := c.Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if devices == nil {
		t.Fatal("Devices returned a nil slice for an empty tailnet, want a non-nil empty slice")
	}
	if len(devices) != 0 {
		t.Fatalf("Devices returned %d devices, want 0", len(devices))
	}
}

// TestANon2xxIsAnErrorNamingTheStatus proves the other half of the same
// distinction: a read that failed must be an error, never quietly turned
// into an empty device list.
func TestANon2xxIsAnErrorNamingTheStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"message": "denied"}`)
			}))
			defer srv.Close()

			c := newTestClient(t, srv.URL)
			devices, err := c.Devices(context.Background())
			if err == nil {
				t.Fatalf("a %d response was not reported as an error", status)
			}
			if devices != nil {
				t.Fatalf("a %d response returned %v, want a nil slice alongside the error", status, devices)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Errorf("error %q does not name the status %d", err.Error(), status)
			}
		})
	}
}

// TestTheAPIKeyNeverAppearsInAnError is the credential-hygiene guard the
// task calls for explicitly: a 401 whose body echoes back the Authorization
// header it received (a realistic "here is what we saw" error page) must
// not leak the key into the error truss logs or alerts on.
func TestTheAPIKeyNeverAppearsInAnError(t *testing.T) {
	const plantedKey = "tskey-api-" + "planted" + "0011223344"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"message": "bad credentials, saw header: %s"}`, auth)
	}))
	defer srv.Close()

	c, err := New(Config{
		APIKey:  plantedKey,
		Tailnet: "example.invalid",
		BaseURL: srv.URL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = c.Devices(context.Background())
	if err == nil {
		t.Fatal("a 401 was not reported as an error")
	}
	if strings.Contains(err.Error(), plantedKey) {
		t.Fatalf("error contains the API key: %v", err)
	}
}

// TestAMalformedBodyIsAnError proves a 200 whose body will not parse as the
// documented shape is refused rather than silently read as "no devices".
func TestAMalformedBodyIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{not json`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	devices, err := c.Devices(context.Background())
	if err == nil {
		t.Fatal("a malformed body was not reported as an error")
	}
	if devices != nil {
		t.Fatalf("a malformed body returned %v, want a nil slice alongside the error", devices)
	}
}

// TestAnOmittedLastSeenIsToldApartByConnectedToControl pins the one place the
// wire format says two opposite things with the same absence.
//
// The published schema omits `lastSeen` BOTH for a device that has never come
// online AND for one connected to the control server right now. Folding them
// together in either direction is wrong, and one direction is dangerous: read
// as "connected", a machine that has never once checked in reports as
// reachable, and the pass would go on to configure a host that is not there.
//
// ⚠️ WITHOUT THIS TEST THE DISTINCTION WAS CORRECT BY ACCIDENT. Replacing the
// `connectedToControl` case with an unconditional "absent means now" left the
// whole suite green, measured 2026-09-10 -- so nothing was holding the
// behaviour in place.
func TestAnOmittedLastSeenIsToldApartByConnectedToControl(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"devices":[
			{"name":"live.tailfe8c.ts.net","tags":["tag:managed"],"connectedToControl":true},
			{"name":"never.tailfe8c.ts.net","tags":["tag:managed"],"connectedToControl":false}
		]}`)
	}))
	defer srv.Close()

	devices, err := newTestClient(t, srv.URL).Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	byName := map[string]Device{}
	for _, d := range devices {
		byName[d.Name] = d
	}

	if _, ok := byName["live"]; !ok {
		t.Fatalf("no device named live in %v", devices)
	}
	if byName["live"].LastSeen.IsZero() {
		t.Error("a device connected to control read as never seen; it cannot be stale by any definition")
	}
	if !byName["never"].LastSeen.IsZero() {
		t.Errorf("a device that has never been online read as seen at %v; absent must not mean connected", byName["never"].LastSeen)
	}

	// And the consequence the distinction exists for: only the one that has
	// never checked in is reported unreachable.
	now := time.Now()
	got := Reconcile(devices, []string{"live", "never"}, "tag:managed", now, time.Hour)
	if len(got.Unreachable) != 1 || got.Unreachable[0] != "never" {
		t.Errorf("Unreachable = %v, want exactly [never]", got.Unreachable)
	}
}

// TestAManagedDeviceIsMatchedToItsDeclaredHostByMagicDNSName is the defect
// the bare-name fixtures hid. The API names a device by its MagicDNS name;
// the inventory names a host by its record. Compared as-is they never match,
// so a declared, connected, managed host reads as BOTH unreachable AND an
// undeclared intruder -- and the intruder half refuses every play in the pass.
//
// Found 2026-09-11 when a host joined a real tailnet: `tailscale whoami`
// reported "<hostname>.<tailnet>.ts.net", the shape the schema documents.
func TestAManagedDeviceIsMatchedToItsDeclaredHostByMagicDNSName(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"devices":[
			{"name":"dev-agent.tailfe8c.ts.net","tags":["tag:managed"],"connectedToControl":true}
		]}`)
	}))
	defer srv.Close()

	devices, err := newTestClient(t, srv.URL).Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	got := Reconcile(devices, []string{"dev-agent"}, "tag:managed", time.Now(), time.Hour)
	if len(got.UnknownTagged) != 0 {
		t.Errorf("UnknownTagged = %v: the declared host dev-agent was not recognised under its MagicDNS name", got.UnknownTagged)
	}
	if len(got.Unreachable) != 0 {
		t.Errorf("Unreachable = %v: a connected device named dev-agent.<tailnet> did not vouch for dev-agent", got.Unreachable)
	}
}

// TestASharedInDeviceNeverVouchesForADeclaredHost pins what matching by the
// first DNS label would otherwise open. A device shared INTO the tailnet
// (isExternal) belongs to someone else's tailnet, and its label is theirs to
// choose -- so "dev-agent.other.ts.net" must not make our dev-agent reachable.
func TestASharedInDeviceNeverVouchesForADeclaredHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"devices":[
			{"name":"dev-agent.other-tailnet.ts.net","tags":[],"isExternal":true,"connectedToControl":true}
		]}`)
	}))
	defer srv.Close()

	devices, err := newTestClient(t, srv.URL).Devices(context.Background())
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	got := Reconcile(devices, []string{"dev-agent"}, "tag:managed", time.Now(), time.Hour)
	if len(got.Unreachable) != 1 || got.Unreachable[0] != "dev-agent" {
		t.Errorf("Unreachable = %v, want exactly [dev-agent]: a shared-in device vouched for a host on this tailnet", got.Unreachable)
	}
}

// TestANameThatIsNotAMagicDNSNameIsRefused keeps the parse honest. A name
// with no domain, or an empty first label, is not the documented shape, and
// guessing a host from it is how a device gets matched to the wrong record.
func TestANameThatIsNotAMagicDNSNameIsRefused(t *testing.T) {
	for _, name := range []string{"web-1", ".tailfe8c.ts.net", ""} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"devices":[{"name":%q,"tags":["tag:managed"],"connectedToControl":true}]}`, name)
			}))
			defer srv.Close()

			devices, err := newTestClient(t, srv.URL).Devices(context.Background())
			if err == nil {
				t.Fatalf("device name %q was accepted as %v, want a refusal", name, devices)
			}
			if !strings.Contains(err.Error(), "MagicDNS") {
				t.Errorf("error %q does not say the name is not a MagicDNS name", err.Error())
			}
		})
	}
}
