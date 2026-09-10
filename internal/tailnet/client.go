// Package tailnet reads Tailscale's own record of which devices are on the
// tailnet, and compares that record, purely, against this repository's
// committed host inventory.
//
// The design rule this package exists under (docs/design.md, and the plan
// it implements) is: git is the source of truth for which machines are
// managed, and the tailnet is an observation, never an input. A machine not
// declared in inventory/hosts/ is not managed, and no tag can make it so --
// otherwise whoever can tag a machine could cause configuration to run on
// it, the same inversion this project already refuses for CI. So Client
// only fetches and decodes; Reconcile only names disagreements. Neither
// changes anything, here or on the tailnet.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// httpTimeout bounds every request this package makes. http.DefaultClient
// has NO timeout, and a host that accepts a connection and never answers
// would hang whatever daily pass eventually calls this client -- the same
// reasoning internal/secrets and internal/forge already record for their
// own clients.
const httpTimeout = 30 * time.Second

// apiHost is the real Tailscale API host, used when Config.BaseURL is
// empty. Verified 2026-09-10 against the live OpenAPI document Tailscale's
// own interactive API reference renders client-side -- fetched directly
// from this host, at the same endpoint and query parameter its frontend
// bundle names for that purpose: `servers[0].url` in that document is
// apiHost plus "/api/v2", and `security` is `bearerAuth` (an HTTP Bearer
// token), not the basic-auth scheme older Tailscale docs mirror. Devices
// carries the request path.
const apiHost = "https://api.tailscale.com"

// Device is one machine as Tailscale's control plane reports it -- an
// observation, never a declaration of what should exist (see the package
// doc and Reconcile).
type Device struct {
	// Name is the wire field "name": the device's MagicDNS name (e.g.
	// "pangolin.tailfe8c.ts.net" in Tailscale's own example). Verified
	// against the `Device` schema in the OpenAPI document cited above.
	// "hostname" is a different, shorter field in the same schema (the
	// admin-console display name) and is not used here.
	Name string
	// Tags is the wire field "tags": every ACL tag applied to the device.
	// A device carrying none of them relevant to this package is not our
	// business -- see Reconcile.
	Tags []string
	// LastSeen is derived from the wire field "lastSeen", which the
	// schema documents as OMITTED -- not zero, absent -- in two cases: a
	// device that has never come online, and a device that is connected
	// to the control server right now ("Omitted if the device has never
	// been online or `connectedToControl` is true"). Devices folds the
	// second case into "seen at the moment of this call", since a
	// currently-connected device cannot be stale by any definition
	// Reconcile uses; the first case is left as the Go zero value, which
	// Reconcile's staleness arithmetic already treats as maximally
	// overdue -- a device that has never checked in should read as
	// unreachable, not as recently seen.
	LastSeen time.Time
}

// wireDevice is the fields this package reads out of the `Device` schema in
// the OpenAPI document cited above. LastSeen is a pointer because its
// absence is a distinct, documented fact (see Device.LastSeen) that a bare
// string could not represent; ConnectedToControl needs no such treatment
// because this package treats its absence and its explicit `false` as the
// same fact -- "not connected right now".
type wireDevice struct {
	Name               string   `json:"name"`
	Tags               []string `json:"tags"`
	LastSeen           *string  `json:"lastSeen"`
	ConnectedToControl bool     `json:"connectedToControl"`
}

// toDevice converts one wire record, using now to stand in for a
// currently-connected device's omitted lastSeen -- see Device.LastSeen.
func (w wireDevice) toDevice(now time.Time) (Device, error) {
	d := Device{Name: w.Name, Tags: w.Tags}
	switch {
	case w.LastSeen != nil:
		t, err := time.Parse(time.RFC3339, *w.LastSeen)
		if err != nil {
			return Device{}, fmt.Errorf("tailnet: device %q: parsing lastSeen %q: %w", w.Name, *w.LastSeen, err)
		}
		d.LastSeen = t
	case w.ConnectedToControl:
		d.LastSeen = now
	}
	return d, nil
}

// Config is everything New needs to reach one tailnet's device list.
type Config struct {
	// APIKey authenticates as an HTTP Bearer token. Never logged, never
	// allowed to reach a returned error string -- see redact.
	APIKey string
	// Tailnet is the tailnet to list, e.g. an organisation name, or "-"
	// for the default tailnet of APIKey (the OpenAPI document's own
	// recommendation for "most users").
	Tailnet string
	// BaseURL overrides the API host. Empty means the real one (apiHost).
	//
	// ⚠️ ITS ABSENCE IS A BUG, NOT A MISSING CONVENIENCE. internal/secrets'
	// Cloudflare probe had no such override, and every test run against it
	// therefore hit the real Cloudflare API. internal/notify.Telegram and
	// internal/forge.Config both carry this for the same reason: it is the
	// only seam that lets a test point this client at an httptest.Server
	// instead of the real tailnet.
	BaseURL string
	// HTTP is the client used for every request. Defaults to a client
	// bounded by httpTimeout when nil; tests point it at an
	// httptest.Server's client.
	HTTP *http.Client
}

// Client lists the devices of one tailnet.
type Client struct {
	apiKey  string
	tailnet string
	baseURL string
	http    *http.Client
}

// New validates cfg and returns a Client, or refuses. APIKey and Tailnet
// must both be non-empty: a Client built on either being empty would either
// fail every request or silently ask the wrong tailnet.
func New(cfg Config) (*Client, error) {
	var problems []string
	if cfg.APIKey == "" {
		problems = append(problems, "APIKey is empty")
	}
	if cfg.Tailnet == "" {
		problems = append(problems, "Tailnet is empty")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("tailnet: refusing to construct a client: %s", strings.Join(problems, "; "))
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = apiHost
	}
	baseURL = strings.TrimRight(baseURL, "/")

	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: httpTimeout}
	}

	return &Client{apiKey: cfg.APIKey, tailnet: cfg.Tailnet, baseURL: baseURL, http: httpClient}, nil
}

// redact replaces every occurrence of a non-empty API key with a fixed
// marker. Applied to response bodies and to stringified errors alike,
// because net/http embeds the request URL in *url.Error and a naive %w
// would carry the Authorization header's key straight through if it ever
// ended up echoed into an error (internal/forge.redact and
// internal/notify.redactToken make the same point for their own secrets).
func redact(s, apiKey string) string {
	if apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, apiKey, "[REDACTED]")
}

// sanitizeErr rebuilds an error from its redacted text rather than wrapping
// the original. Wrapping is not enough: %w keeps the original error's
// Error() reachable, and a *url.Error's Error() method re-renders the
// request URL every time it is called, so any redaction applied to a
// one-off .Error() string would not survive being wrapped
// (internal/forge.sanitizeErr's comment records the same finding).
func sanitizeErr(err error, apiKey string) error {
	if err == nil {
		return nil
	}
	return errors.New(redact(err.Error(), apiKey))
}

// Devices lists every device on the configured tailnet. A non-2xx response
// is an error naming the status -- never an empty slice, which this method
// reserves for the tailnet genuinely having no devices (internal/secrets'
// Store.List records why the two must never be confused: an unreadable
// tailnet is not the same fact as an empty one).
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	path := c.baseURL + "/api/v2/tailnet/" + url.PathEscape(c.tailnet) + "/devices"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, fmt.Errorf("tailnet: building the device list request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, sanitizeErr(err, c.apiKey)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, sanitizeErr(err, c.apiKey)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("tailnet: listing devices for %q: %d: %s", c.tailnet, resp.StatusCode, redact(string(body), c.apiKey))
	}

	var parsed struct {
		Devices []wireDevice `json:"devices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("tailnet: decoding device list for %q: %w", c.tailnet, err)
	}

	now := time.Now()
	devices := make([]Device, 0, len(parsed.Devices))
	for _, w := range parsed.Devices {
		d, err := w.toDevice(now)
		if err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, nil
}
