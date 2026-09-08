package ledger

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAgainstTheRealBucket is §5.4: the only test that answers the
// addressing-style and region questions §8 raised, by writing and reading
// through both this package AND the reference tool (`aws s3api`) and
// byte-comparing in both directions. It needs credentials that live in a
// cluster this package was written outside of, so it is opt-in behind
// TRUSS_LEDGER_LIVE=1 and is not run as part of this change -- see the
// coordinator's note: addressing and region are now measured (path-style,
// us-east-1) and set as this package's defaults; what remains open is
// whether the endpoint honours "If-None-Match: *" under HMAC auth, which
// only a live run against the real bucket can answer.
//
// Configuration comes entirely from the environment -- nothing here names
// the real bucket, vault or endpoint, in keeping with the rule that no
// fixture in this repository may (leakscan enforces this over every
// tracked file).
func TestAgainstTheRealBucket(t *testing.T) {
	if os.Getenv("TRUSS_LEDGER_LIVE") != "1" {
		t.Skip("set TRUSS_LEDGER_LIVE=1 to run this against the real bucket; it needs credentials this environment does not have")
	}

	endpoint := requireEnv(t, "TRUSS_LEDGER_TEST_ENDPOINT")
	bucket := requireEnv(t, "TRUSS_LEDGER_TEST_BUCKET")
	region := requireEnv(t, "TRUSS_LEDGER_TEST_REGION")
	accessKeyID := requireEnv(t, "TRUSS_LEDGER_TEST_ACCESS_KEY_ID")
	secretAccessKey := requireEnv(t, "TRUSS_LEDGER_TEST_SECRET_ACCESS_KEY")
	prefix := requireEnv(t, "TRUSS_LEDGER_TEST_PREFIX") // a SCRATCH prefix, never the real ledger's

	if _, err := exec.LookPath("aws"); err != nil {
		t.Fatalf("aws CLI not found on PATH: %v", err)
	}

	cfg := Config{
		Endpoint:        endpoint,
		Bucket:          bucket,
		Region:          region,
		Addressing:      PathStyle,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	}
	store, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	awsEnv := append(os.Environ(),
		"AWS_ACCESS_KEY_ID="+accessKeyID,
		"AWS_SECRET_ACCESS_KEY="+secretAccessKey,
		// The AWS CLI v2 sends x-amz-checksum-* headers on every upload by
		// default. Google's S3-compatible XML API rejects them with
		// "SignatureDoesNotMatch: Invalid argument", so without these two the
		// CLI cannot write to this endpoint at all and the parity direction
		// that uses it fails for a reason that has nothing to do with our
		// signing. Measured in-cluster 2026-09-08: 3/3 aws-writes subtests
		// failed this way, while all 3 go-writes subtests passed.
		"AWS_REQUEST_CHECKSUM_CALCULATION=when_required",
		"AWS_RESPONSE_CHECKSUM_VALIDATION=when_required",
	)

	bodies := map[string][]byte{
		"empty": {},
		"nul":   {0x00, 'a', 0x00, 'b', 0x00},
		"1mib":  bytes.Repeat([]byte("truss-ledger-parity-"), (1<<20)/len("truss-ledger-parity-")+1)[:1<<20],
	}

	for name, body := range bodies {
		t.Run(name+"/go-writes-aws-reads", func(t *testing.T) {
			key := fmt.Sprintf("%s/parity/%s-%s", prefix, name, randomHex(t))
			ctx := context.Background()
			if err := store.Put(ctx, key, body); err != nil {
				t.Fatalf("Store.Put: %v", err)
			}
			got := awsGetObject(t, awsEnv, endpoint, bucket, key)
			if !bytes.Equal(got, body) {
				t.Errorf("aws s3api get-object read back %d bytes, want %d bytes matching what Store.Put wrote", len(got), len(body))
			}
		})

		t.Run(name+"/aws-writes-go-reads", func(t *testing.T) {
			key := fmt.Sprintf("%s/parity/%s-%s", prefix, name, randomHex(t))
			awsPutObject(t, awsEnv, endpoint, bucket, key, body)
			got, err := store.Get(context.Background(), key)
			if err != nil {
				t.Fatalf("Store.Get: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Errorf("Store.Get read back %d bytes, want %d bytes matching what aws s3api put-object wrote", len(got), len(body))
			}
		})
	}

	// Not a feature test -- a standing measurement. The ledger deliberately
	// offers no create-if-absent (see store.go) because this endpoint accepts
	// "If-None-Match: *" and ignores it. If that ever changes, this fails and
	// the decision is worth revisiting; until then it stops anyone concluding
	// from the S3 docs that the precondition is available here.
	t.Run("if-none-match/is-accepted-and-ignored", func(t *testing.T) {
		key := fmt.Sprintf("%s/parity/ifnonematch-%s", prefix, randomHex(t))
		ctx := context.Background()
		hdr := map[string]string{"If-None-Match": "*"}
		r1, err := store.do(ctx, "PUT", key, []byte("first"), hdr)
		if err != nil {
			t.Fatalf("first conditional write: %v", err)
		}
		r1.Body.Close()
		r2, err := store.do(ctx, "PUT", key, []byte("second"), hdr)
		if err != nil {
			t.Fatalf("second conditional write: %v", err)
		}
		defer r2.Body.Close()
		if r2.StatusCode == http.StatusPreconditionFailed || r2.StatusCode == http.StatusConflict {
			t.Fatalf("the endpoint now ENFORCES If-None-Match (status %d) -- it did not on 2026-09-08. A real create-if-absent is available again; revisit store.go", r2.StatusCode)
		}
		got, err := store.Get(ctx, key)
		if err != nil {
			t.Fatalf("Store.Get: %v", err)
		}
		if string(got) != "second" {
			t.Fatalf("second write left %q; expected the endpoint to have overwritten with \"second\"", got)
		}
	})

	// x-goog-if-generation-match is the native create-if-absent and is
	// unreachable from here: SigV4 forces x-amz-date and x-amz-content-sha256
	// onto every request, and the endpoint refuses any request carrying both
	// header families.
	t.Run("x-goog-header/cannot-be-mixed-with-sigv4", func(t *testing.T) {
		key := fmt.Sprintf("%s/parity/xgoog-%s", prefix, randomHex(t))
		resp, err := store.do(context.Background(), "PUT", key, []byte("x"), map[string]string{"x-goog-if-generation-match": "0"})
		if err != nil {
			t.Fatalf("conditional write: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(body), "ExcessHeaderValues") {
			t.Fatalf("x-goog-if-generation-match now returns %d %q -- on 2026-09-08 it was 400 ExcessHeaderValues. Revisit store.go", resp.StatusCode, body)
		}
	})
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("TRUSS_LEDGER_LIVE=1 but %s is unset", name)
	}
	return v
}

func randomHex(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}

func awsGetObject(t *testing.T, env []string, endpoint, bucket, key string) []byte {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "ledger-live-get-")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmp.Close()

	cmd := exec.Command("aws", "s3api", "get-object",
		"--endpoint-url", endpoint, "--bucket", bucket, "--key", key, tmp.Name())
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("aws s3api get-object: %v\n%s", err, out)
	}
	body, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("reading aws's output file: %v", err)
	}
	return body
}

func awsPutObject(t *testing.T, env []string, endpoint, bucket, key string, body []byte) {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "ledger-live-put-")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer tmp.Close()
	if _, err := tmp.Write(body); err != nil {
		t.Fatalf("writing temp body: %v", err)
	}

	cmd := exec.Command("aws", "s3api", "put-object",
		"--endpoint-url", endpoint, "--bucket", bucket, "--key", key, "--body", tmp.Name())
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("aws s3api put-object: %v\n%s", err, out)
	}
}
