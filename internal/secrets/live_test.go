package secrets

import (
	"context"
	"os"
	"testing"
)

// TestAgainstTheRealVault is opt-in behind TRUSS_VAULT_LIVE=1 and is not
// run as part of this change -- it needs a live Vault, a real Kubernetes
// ServiceAccount JWT and a role that can actually log in, none of which
// this environment has. It is written so a future session pointed at a
// real cluster (or `vault server -dev`) has something to run rather than
// something to write.
//
// Metadata reads only: it never writes to Vault, matching the applier's
// own read-only policy (vault/bootstrap-vault.sh, "a compromised applier
// pod cannot rewrite the store it reads from") and this package's own
// refusal to do so.
func TestAgainstTheRealVault(t *testing.T) {
	if os.Getenv("TRUSS_VAULT_LIVE") != "1" {
		t.Skip("set TRUSS_VAULT_LIVE=1 to run this against a real Vault; it needs a cluster this environment does not have")
	}

	addr := requireLiveEnv(t, "TRUSS_VAULT_TEST_ADDR")
	mount := requireLiveEnv(t, "TRUSS_VAULT_TEST_MOUNT")
	role := requireLiveEnv(t, "TRUSS_VAULT_TEST_ROLE")
	jwtPath := requireLiveEnv(t, "TRUSS_VAULT_TEST_JWT_PATH")
	// An item this role can read the metadata of, known to carry an
	// `expires` field -- proof that a real read comes back recorded=true
	// and not just that the request didn't error.
	knownItem := requireLiveEnv(t, "TRUSS_VAULT_TEST_KNOWN_ITEM")

	kv, err := NewKV(KVConfig{Addr: addr, Mount: mount, Role: role, JWTPath: jwtPath})
	if err != nil {
		t.Fatalf("NewKV: %v", err)
	}

	ctx := context.Background()
	items, err := kv.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	haveKnownItem := false
	for _, item := range items {
		if item == knownItem {
			haveKnownItem = true
		}
	}
	if !haveKnownItem {
		t.Fatalf("List of %s did not include %q -- TRUSS_VAULT_TEST_KNOWN_ITEM points at the wrong mount or item", mount, knownItem)
	}

	raw, recorded, err := kv.Expiry(ctx, knownItem)
	if err != nil {
		t.Fatalf("Expiry(%s): %v", knownItem, err)
	}
	if !recorded {
		t.Fatalf("Expiry(%s) recorded=false, want true for a known-seeded item", knownItem)
	}
	if raw == "" {
		t.Fatalf("Expiry(%s) recorded=true but raw is empty", knownItem)
	}
}

func requireLiveEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("TRUSS_VAULT_LIVE=1 but %s is unset", name)
	}
	return v
}
