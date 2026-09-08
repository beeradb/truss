package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestLedgerGetExitsTwoWhenAbsentAndOneOnAnyOtherError is the exit-code
// contract §4.9 states in words: "0 found, 2 absent, 1 any other error --
// this is what lets apply.sh keep `|| die` for HEAD and refuse-with-a-true-
// reason for a digest." The two failure codes must stay distinct, so this
// test drives both, plus the success path, through the same command.
func TestLedgerGetExitsTwoWhenAbsentAndOneOnAnyOtherError(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	fl.objects["present-key"] = []byte("hello")

	t.Run("found", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runEnv(context.Background(), []string{"ledger", "get", "present-key"}, env, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
		}
		if stdout.String() != "hello" {
			t.Fatalf("stdout = %q, want %q", stdout.String(), "hello")
		}
	})

	t.Run("absent", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := runEnv(context.Background(), []string{"ledger", "get", "missing-key"}, env, nil, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr.String())
		}
	})

	t.Run("any other error", func(t *testing.T) {
		// Point at a server that answers every request with a 500 --
		// "could not look", never mistaken for "absent" (§3.2).
		errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer errSrv.Close()

		dir2, write2 := testSecretsDir(t)
		write2(itemLedger, fieldLedgerEndpoint, errSrv.URL)
		write2(itemLedger, fieldLedgerAccessKey, "AKIAFAKEACCESSKEYID")
		write2(itemLedger, fieldLedgerSecretKey, "fakesecretaccesskeyfakesecretaccesskey")
		env2 := testFullEnv(dir2.Root, t.TempDir(), nil)

		var stdout, stderr bytes.Buffer
		code := runEnv(context.Background(), []string{"ledger", "get", "any-key"}, env2, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stderr: %s)", code, stderr.String())
		}
	})
}

// TestLedgerGetExitsTwoWhenAbsentAndOneOnAnyOtherError_Breakable documents
// the guard: swapping the two branches in cmdLedger's `get` case (returning
// 1 for absent and 2 for any other error) makes the subtests above fail on
// their own exit-code assertions, not on a setup error -- confirmed by hand
// while writing this test, then restored. See the final report.
func TestLedgerPutWritesExactBytes(t *testing.T) {
	dir, write := testSecretsDir(t)
	fl := newFakeLedger(t, "state-bucket")
	writeLedgerSecret(t, write, fl)
	env := testFullEnv(dir.Root, t.TempDir(), nil)

	var stdout, stderr bytes.Buffer
	code := runEnv(context.Background(), []string{"ledger", "put", "some-key"}, env, strings.NewReader("payload"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	got, ok := fl.get("some-key")
	if !ok || string(got) != "payload" {
		t.Fatalf("stored object = %q, %v, want %q, true", got, ok, "payload")
	}
}
