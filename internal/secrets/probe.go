package secrets

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Probe asks an issuer directly for one credential's expiry, rather than
// reading it out of Vault. The Cloudflare API is asked first, for the one
// hand-made token (cf-token-mint) whose true answer only Cloudflare has: a
// value written to Vault could go stale in a way the issuer's own answer
// cannot.
type Probe interface {
	Expiry(ctx context.Context) (t time.Time, ok bool, err error)
}

// CloudflareToken probes Cloudflare's own token-verification endpoint for
// the hand-made cf-infra-admin minting token's own expiry (see Expiry).
type CloudflareToken struct {
	// BaseURL overrides the Cloudflare API host for tests. Empty means the
	// real one.
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// Expiry asks Cloudflare directly. A transport failure, a non-2xx status
// or a body that will not parse all report as (zero, false, nil) -- a
// probe failure is "no expiry recorded", not an error that could halt the
// sweep (§4.7). Asking the issuer is an improvement on what Vault holds,
// and an improvement that is unavailable must not cost the sweep every
// other credential it was about to report on.
func (c CloudflareToken) Expiry(ctx context.Context) (time.Time, bool, error) {
	base := c.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com"
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/client/v4/user/tokens/verify", nil)
	if err != nil {
		return time.Time{}, false, nil
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, false, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return time.Time{}, false, nil
	}

	var parsed struct {
		Result struct {
			ExpiresOn string `json:"expires_on"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Result.ExpiresOn == "" {
		return time.Time{}, false, nil
	}

	t, err := time.Parse(time.RFC3339, parsed.Result.ExpiresOn)
	if err != nil {
		return time.Time{}, false, nil
	}
	return t, true, nil
}
