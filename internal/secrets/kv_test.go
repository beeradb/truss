package secrets

import (
	"context"
	"strings"
	"testing"
)

func TestNewKVRefusesAnEmptyAddressRoleOrMount(t *testing.T) {
	base := KVConfig{Addr: "http://vault.example", Mount: "platform", Role: "applier", JWTPath: "/var/run/secrets/vault-token/token"}

	cases := []struct {
		name string
		cfg  KVConfig
	}{
		{"empty Addr", func() KVConfig { c := base; c.Addr = ""; return c }()},
		{"empty Mount", func() KVConfig { c := base; c.Mount = ""; return c }()},
		{"empty Role", func() KVConfig { c := base; c.Role = ""; return c }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewKV(tc.cfg)
			if err == nil {
				t.Fatalf("NewKV(%+v) = nil error, want a refusal", tc.cfg)
			}
		})
	}

	if _, err := NewKV(base); err != nil {
		t.Fatalf("NewKV with every field set = %v, want no error", err)
	}
}

func TestExactlyOneLoginPerRun(t *testing.T) {
	fv := newFakeVault()
	fv.items = []string{"a", "b"}
	fv.meta["a"] = fakeItemMeta{recorded: true, expires: "never"}
	fv.meta["b"] = fakeItemMeta{recorded: true, expires: "2099-01-01"}
	srv := fv.server()
	defer srv.Close()

	kv := newTestKV(t, fv, srv)
	ctx := context.Background()

	if _, err := kv.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, _, err := kv.Expiry(ctx, "a"); err != nil {
		t.Fatalf("Expiry(a): %v", err)
	}
	if _, _, err := kv.Expiry(ctx, "b"); err != nil {
		t.Fatalf("Expiry(b): %v", err)
	}

	if got := fv.loginCount(); got != 1 {
		t.Errorf("login count = %d, want exactly 1 across a List and two Expiry calls", got)
	}
}

func TestALoginFailureIsAnErrorNotAnEmptySweep(t *testing.T) {
	fv := newFakeVault()
	fv.loginStatus = 403
	fv.items = []string{"a"}
	srv := fv.server()
	defer srv.Close()

	kv := newTestKV(t, fv, srv)

	items, err := kv.List(context.Background())
	if err == nil {
		t.Fatalf("List with a failed login = (%v, nil), want an error", items)
	}
	if items != nil {
		t.Errorf("List on a login failure returned %v, want nil", items)
	}
}

func TestListRefusesToReportAnEmptyVaultItCouldNotRead(t *testing.T) {
	t.Run("permission denied is an error, not an empty list", func(t *testing.T) {
		fv := newFakeVault()
		fv.listStatus = 403
		srv := fv.server()
		defer srv.Close()

		kv := newTestKV(t, fv, srv)
		items, err := kv.List(context.Background())
		if err == nil {
			t.Fatalf("List on a 403 = (%v, nil), want an error", items)
		}
	})

	t.Run("a genuinely empty mount is not an error", func(t *testing.T) {
		fv := newFakeVault() // no items at all
		srv := fv.server()
		defer srv.Close()

		kv := newTestKV(t, fv, srv)
		items, err := kv.List(context.Background())
		if err != nil {
			t.Fatalf("List on a genuinely empty mount = %v, want no error", err)
		}
		if len(items) != 0 {
			t.Errorf("List on an empty mount = %v, want none", items)
		}
	})
}

func TestAMetadataReadFailureIsNeverNoExpiryRecorded(t *testing.T) {
	fv := newFakeVault()
	fv.items = []string{"broken-item"}
	fv.metaStatus["broken-item"] = 500
	srv := fv.server()
	defer srv.Close()

	kv := newTestKV(t, fv, srv)
	raw, recorded, err := kv.Expiry(context.Background(), "broken-item")
	if err == nil {
		t.Fatalf("Expiry on a failed metadata read = (%q, %v, nil), want an error", raw, recorded)
	}
}

func TestTheSweepReadsNoSecretData(t *testing.T) {
	fv := newFakeVault()
	fv.items = []string{"a", "b"}
	fv.meta["a"] = fakeItemMeta{recorded: true, expires: "2099-01-01"}
	fv.meta["b"] = fakeItemMeta{}
	srv := fv.server()
	defer srv.Close()

	kv := newTestKV(t, fv, srv)
	ctx := context.Background()
	if _, err := kv.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range fv.items {
		if _, _, err := kv.Expiry(ctx, item); err != nil {
			t.Fatalf("Expiry(%s): %v", item, err)
		}
	}

	for _, p := range fv.requestPaths() {
		if strings.Contains(p, "/data/") {
			t.Errorf("request path %q touched /data/ -- the sweep must read nothing but metadata", p)
		}
	}
}

func TestOnlyExpiresIsEverReadFromARuntimeVault(t *testing.T) {
	fv := newFakeVault()
	fv.items = []string{"a"}
	fv.meta["a"] = fakeItemMeta{
		recorded: true,
		expires:  "2099-01-01",
		extra:    map[string]string{"owner": "should-never-appear-in-a-finding"},
	}
	srv := fv.server()
	defer srv.Close()

	kv := newTestKV(t, fv, srv)
	raw, recorded, err := kv.Expiry(context.Background(), "a")
	if err != nil {
		t.Fatalf("Expiry: %v", err)
	}
	if !recorded || raw != "2099-01-01" {
		t.Fatalf("Expiry = (%q, %v), want (\"2099-01-01\", true)", raw, recorded)
	}
	if strings.Contains(raw, "should-never-appear") {
		t.Errorf("Expiry's raw value carried a custom_metadata field other than expires: %q", raw)
	}
}

func TestTheClientTokenAndTheJWTNeverAppearInAnError(t *testing.T) {
	fv := newFakeVault()
	fv.loginReply = authReplyToken()
	fv.listStatus = 500 // force an error after a successful login
	srv := fv.server()
	defer srv.Close()

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

	_, err = kv.List(context.Background())
	if err == nil {
		t.Fatal("List with listStatus=500 = nil error, want an error to inspect")
	}
	msg := err.Error()
	if strings.Contains(msg, fv.loginReply) {
		t.Errorf("error %q contains the client token", msg)
	}
	if strings.Contains(msg, jwt) {
		t.Errorf("error %q contains the JWT", msg)
	}
}

func TestNoMountRoleOrItemNameIsHardcoded(t *testing.T) {
	fv := newFakeVault()
	fv.items = []string{"an-unusual-item-name"}
	fv.meta["an-unusual-item-name"] = fakeItemMeta{recorded: true, expires: "never"}
	srv := fv.server()
	defer srv.Close()

	jwt := jwtFixture()
	jwtPath := writeJWTFixture(t, jwt)
	kv, err := NewKV(KVConfig{
		Addr:    srv.URL,
		Mount:   "a-very-unusual-mount-name",
		Role:    "a-very-unusual-role-name",
		JWTPath: jwtPath,
		HTTP:    srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewKV: %v", err)
	}

	if _, err := kv.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, _, err := kv.Expiry(context.Background(), "an-unusual-item-name"); err != nil {
		t.Fatalf("Expiry: %v", err)
	}

	found := false
	for _, p := range fv.requestPaths() {
		if strings.Contains(p, "a-very-unusual-mount-name") && strings.Contains(p, "an-unusual-item-name") {
			found = true
		}
	}
	if !found {
		t.Error("no request path carried both the configured mount and item name -- something is hardcoded")
	}
}
