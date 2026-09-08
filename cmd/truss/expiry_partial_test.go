package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ⚠️ A SWEEP THAT CANNOT FINISH MUST STILL REPORT WHAT IT LEARNED.
//
// Sweep.Run evaluates the live PROBES before it walks any store, and returns
// those findings alongside its error. cmdExpiry used to discard them, so one
// store recording no expiries suppressed the Cloudflare probe's answer --
// which, until the rotation write-back lands, is the ONLY real expiry data
// this system has. Measured against production 2026-09-08: `truss expiry`
// printed nothing but "platform lists 6 item(s) but not one records an
// expiry", and exited 1.
//
// This must not soften §4.7: the exit code stays 1 and the error still reaches
// stderr, so nothing can read the output as a clean bill of health. The point
// is only that failing and forgetting are different things.

// fakeCloudflareExpiring answers the token-verify endpoint with a token that
// expires soon enough to be a finding.
func fakeCloudflareExpiring(t *testing.T, expiresOn time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"success":true,"result":{"expires_on":%q}}`,
			expiresOn.UTC().Format(time.RFC3339))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestExpiryReportsProbeFindingsEvenWhenAStoreFails(t *testing.T) {
	// A mount that LISTS items but records an expiry on none of them: the
	// production state today, and the thing that made the sweep fail.
	vaultSrv := productionShapedVault(t, "platform")
	cfSrv := fakeCloudflareExpiring(t, time.Now().Add(5*24*time.Hour))

	dir, write := testSecretsDir(t)
	write(itemCFTokenMint, fieldCFCredential, "fake-mint-token")

	env := map[string]string{
		"REPO": "acme/platform", "APPROVER": "alice",
		"LEDGER_BUCKET": "state-bucket", "LEDGER_APPLIED_PREFIX": "applied",
		"LEDGER_FAILED_PREFIX": "failed", "LEDGER_HEAD_KEY": "head",
		"HEARTBEAT_KEY": "heartbeat", "PLAN_DIGEST_PREFIX": "digests",
		"WORKDIR":                 t.TempDir(),
		"OP_TOKEN_FILE":           testJWTFile(t),
		"SECRETS_DIR":             dir.Root,
		"VAULT_ADDR":              vaultSrv.URL,
		"VAULT_ROLE":              "applier",
		"VAULT_MOUNT":             "platform",
		"VAULT_JWT_PATH":          testJWTFile(t),
		"CLOUDFLARE_API_BASE_URL": cfSrv.URL,
	}
	getenv := func(k string) string { return env[k] }

	var out, errOut bytes.Buffer
	code := cmdExpiry(context.Background(), nil, getenv, &out, &errOut)

	// ⚠️ Still a failure. A partial answer must never be mistaken for a clean
	// sweep, which is why this assertion comes first.
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 -- a sweep that could not finish must fail\nstderr: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "not one records an expiry") {
		t.Fatalf("the real failure was not reported on stderr:\n%s", errOut.String())
	}

	// ...and it must not have forgotten the probe.
	var findings []map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &findings); err != nil {
		t.Fatalf("no parsable findings on stdout (%v); the probe's answer was discarded.\nstdout: %q\nstderr: %s",
			err, out.String(), errOut.String())
	}
	if len(findings) == 0 {
		t.Fatal("findings were empty; the Cloudflare probe's answer was discarded")
	}
	found := false
	for _, f := range findings {
		if f["name"] == itemCFTokenMint {
			found = true
		}
	}
	if !found {
		t.Fatalf("cf-token-mint was not among the reported findings: %v", findings)
	}
}
