package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// httpTimeout bounds every request this package makes. http.DefaultClient
// has NO timeout, so a server that accepts a connection and then never
// answers hangs the caller forever -- and the caller here is a CronJob that
// fires every five minutes, so a hung pass is a pile of pods rather than one
// stuck process. 30s matches internal/forge, which was the only package that
// had bounded itself. Raised as an inconsistency by the 2026-09-08 security
// review.
const httpTimeout = 30 * time.Second

// Store is one Vault mount's worth of expiry metadata. It replaces the
// Vault interface and the `op` CLI: metadata only, it cannot return a
// secret value, because the sweep's whole premise is that it reads nothing
// a compromise of it could leak (§4.7).
type Store interface {
	// Name identifies the mount for error messages and for the "reported
	// twice" behaviour when the same title exists in two mounts.
	Name() string
	// List returns every item title under the mount. A mount that is
	// genuinely empty returns (nil, nil); a mount that could not be read
	// -- a failed login already surfaced by an earlier call, a permission
	// error, a 5xx -- returns a non-nil error. The two must never be
	// confused: an unreadable vault is not the same fact as an empty one
	// (§4.7 "Refuses to").
	List(ctx context.Context) ([]string, error)
	// Expiry reads one item's `expires` custom-metadata field. recorded is
	// true only when the item's metadata actually carries an `expires`
	// key -- including when its value is "never" -- and false when the
	// key is simply absent. A failed read is an error, never
	// ("", false, nil): that triple is reserved for a metadata read that
	// genuinely succeeded and found nothing, and a network or permission
	// failure must not be indistinguishable from it
	// (TestAMetadataReadFailureIsNeverNoExpiryRecorded).
	Expiry(ctx context.Context, item string) (raw string, recorded bool, err error)
}

// KVConfig is everything NewKV needs to reach one Vault KV v2 mount,
// authenticating with a projected Kubernetes ServiceAccount JWT the way
// `vault write auth/kubernetes/login role=applier jwt=@...` already does
// in the init container (20-cronjob.yaml:117-118).
type KVConfig struct {
	// Addr is the scheme and host, e.g. "http://vault.vault.svc:8200". No
	// trailing slash.
	Addr string
	// Mount is the KV v2 mount path, e.g. "platform". Never hardcoded
	// inside this package (TestNoMountRoleOrItemNameIsHardcoded).
	Mount string
	// Role is the Kubernetes auth role to log in as, e.g. "applier".
	Role string
	// JWTPath is the path to the projected ServiceAccount token, e.g.
	// "/var/run/secrets/vault-token/token".
	JWTPath string
	// HTTP is the client used for every request. Defaults to
	// http.DefaultClient when nil; tests point it at an httptest.Server.
	HTTP *http.Client
}

// KV is a Store backed by one Vault KV v2 mount. It logs in at most once
// per instance, on the first call that needs a token, and never refreshes
// or retries: a fresh KV is what a fresh Run constructs, so "at most one
// login" here is the same promise as "never cache a token beyond one Run"
// at the level that matters.
type KV struct {
	cfg   KVConfig
	http  *http.Client
	token string
	jwt   string // held only long enough to redact it out of a login error
}

// NewKV validates cfg and returns a KV, or refuses. Addr, Mount, Role and
// JWTPath must all be non-empty: a KV built on any of them being empty
// would either fail every request or silently talk to the wrong mount.
func NewKV(cfg KVConfig) (*KV, error) {
	var problems []string
	if cfg.Addr == "" {
		problems = append(problems, "Addr is empty")
	}
	if cfg.Mount == "" {
		problems = append(problems, "Mount is empty")
	}
	if cfg.Role == "" {
		problems = append(problems, "Role is empty")
	}
	if cfg.JWTPath == "" {
		problems = append(problems, "JWTPath is empty")
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("secrets: refusing to construct a KV store: %s", strings.Join(problems, "; "))
	}

	cfg.Addr = strings.TrimRight(cfg.Addr, "/")
	k := &KV{cfg: cfg, http: cfg.HTTP}
	if k.http == nil {
		k.http = &http.Client{Timeout: httpTimeout}
	}
	return k, nil
}

// Name returns the mount name, so a sweep error and a "reported in two
// mounts" finding can both name where it came from.
func (k *KV) Name() string { return k.cfg.Mount }

// login authenticates via Kubernetes auth and caches the client token for
// the lifetime of this KV. It is a no-op on every call after the first.
func (k *KV) login(ctx context.Context) error {
	if k.token != "" {
		return nil
	}

	jwtBytes, err := os.ReadFile(k.cfg.JWTPath)
	if err != nil {
		return fmt.Errorf("secrets: reading the ServiceAccount JWT at %s: %w", k.cfg.JWTPath, err)
	}
	k.jwt = strings.TrimSpace(string(jwtBytes))

	reqBody, err := json.Marshal(struct {
		Role string `json:"role"`
		JWT  string `json:"jwt"`
	}{Role: k.cfg.Role, JWT: k.jwt})
	if err != nil {
		return fmt.Errorf("secrets: encoding the login request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.cfg.Addr+"/v1/auth/kubernetes/login", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("secrets: building the login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := k.http.Do(req)
	if err != nil {
		return fmt.Errorf("secrets: logging in to vault at %s: %s", k.cfg.Addr, redact(err.Error(), k.jwt))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("secrets: vault login as role %q returned %d: %s", k.cfg.Role, resp.StatusCode, redact(string(body), k.jwt))
	}

	var parsed struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("secrets: parsing the vault login response: %w", err)
	}
	if parsed.Auth.ClientToken == "" {
		return fmt.Errorf("secrets: vault login as role %q returned no client_token", k.cfg.Role)
	}

	k.token = parsed.Auth.ClientToken
	return nil
}

// redact strips this KV's own token and JWT out of a string before it can
// reach a caller in an error.
func (k *KV) redact(s string) string { return redact(s, k.token, k.jwt) }

// List returns every item title in the mount, reading
// <mount>/metadata/ -- never <mount>/data/ (§4.7's load-bearing
// invariant: "reads nothing but the expiry" is a property of the URL).
//
// Vault answers a LIST with 404 when nothing exists under the prefix; that
// is Vault's own way of saying "zero items", not a failure, so it is the
// one non-2xx status this method does not turn into an error.
func (k *KV) List(ctx context.Context) ([]string, error) {
	if err := k.login(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "LIST", k.cfg.Addr+"/v1/"+k.cfg.Mount+"/metadata", nil)
	if err != nil {
		return nil, fmt.Errorf("secrets: building the list request for %s: %w", k.cfg.Mount, err)
	}
	req.Header.Set("X-Vault-Token", k.token)

	resp, err := k.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("secrets: listing %s: %s", k.cfg.Mount, k.redact(err.Error()))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("secrets: listing %s: vault returned %d: %s", k.cfg.Mount, resp.StatusCode, k.redact(string(body)))
	}

	var parsed struct {
		Data struct {
			Keys []string `json:"keys"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("secrets: parsing the list response for %s: %w", k.cfg.Mount, err)
	}
	return parsed.Data.Keys, nil
}

// Expiry reads <mount>/metadata/<item> and returns its `expires`
// custom-metadata field. A read that fails -- login, transport, a non-2xx
// status, a body that will not parse -- returns an error; it never
// degrades to ("", false, nil), which this package reserves for a read
// that genuinely succeeded and found no `expires` key.
func (k *KV) Expiry(ctx context.Context, item string) (raw string, recorded bool, err error) {
	if err := k.login(ctx); err != nil {
		return "", false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.cfg.Addr+"/v1/"+k.cfg.Mount+"/metadata/"+item, nil)
	if err != nil {
		return "", false, fmt.Errorf("secrets: building the metadata request for %q in %s: %w", item, k.cfg.Mount, err)
	}
	req.Header.Set("X-Vault-Token", k.token)

	resp, err := k.http.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("secrets: reading metadata for %q in %s: %s", item, k.cfg.Mount, k.redact(err.Error()))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return "", false, fmt.Errorf("secrets: reading metadata for %q in %s: vault returned %d: %s", item, k.cfg.Mount, resp.StatusCode, k.redact(string(body)))
	}

	var parsed struct {
		Data struct {
			CustomMetadata map[string]string `json:"custom_metadata"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", false, fmt.Errorf("secrets: parsing metadata for %q in %s: %w", item, k.cfg.Mount, err)
	}

	value, ok := parsed.Data.CustomMetadata["expires"]
	return value, ok, nil
}
