package ledger

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestRetentionIsEnforcedAgainstSigV4Writes answers the one question that
// decides whether a locked retention policy can protect this ledger at all:
// does Cloud Storage enforce a BUCKET-level retention policy against writes
// that arrive through the S3-compatible XML API, signed with AWS SigV4?
//
// ⚠️ IT IS A MEASUREMENT, NOT A UNIT TEST, AND THE ANSWER IS NOT YET KNOWN.
// The reasoning that says yes is architectural -- the interop surface is an
// alternate auth and wire format onto the same objects, and the retention
// check lives in the storage layer rather than in an API front end -- and no
// sentence in Google's documentation was found stating it. This package has
// been wrong about GCS interop twice on exactly that kind of reasoning: the
// "If-None-Match: *" precondition is accepted and then ignored (see Put), and
// the AWS CLI's default checksum headers are rejected outright (see
// TestAgainstTheRealBucket). Neither was predictable from the docs. So this
// is written to be RUN, and its answer belongs in a comment here afterwards,
// the way those two answers are recorded where they were measured.
//
// ⚠️ POINT IT AT A SCRATCH BUCKET. A retention policy cannot be shortened
// once locked and this test deliberately writes a second time to a key it
// just wrote. TRUSS_LEDGER_TEST_RETENTION_BUCKET must be a bucket created for
// this and nothing else, with an UNLOCKED retention policy -- an unlocked
// policy is enough to answer the question and can be removed afterwards.
// Locking is what cannot be undone, and this test never needs it.
//
// Set it up out of band, since configuring retention is not something this
// package's client can do (the S3 XML API has no equivalent):
//
//	gcloud storage buckets create gs://<scratch> --location=<loc>
//	gcloud storage buckets update gs://<scratch> --retention-period=60s
//
// Then run:
//
//	TRUSS_LEDGER_LIVE=1 \
//	TRUSS_LEDGER_TEST_ENDPOINT=... TRUSS_LEDGER_TEST_RETENTION_BUCKET=... \
//	TRUSS_LEDGER_TEST_REGION=... TRUSS_LEDGER_TEST_ACCESS_KEY_ID=... \
//	TRUSS_LEDGER_TEST_SECRET_ACCESS_KEY=... TRUSS_LEDGER_TEST_PREFIX=scratch/ \
//	go test -count=1 -run TestRetentionIsEnforcedAgainstSigV4Writes ./internal/ledger/
//
// Afterwards: gcloud storage buckets update gs://<scratch> --clear-retention-period
func TestRetentionIsEnforcedAgainstSigV4Writes(t *testing.T) {
	if os.Getenv("TRUSS_LEDGER_LIVE") != "1" {
		t.Skip("set TRUSS_LEDGER_LIVE=1 to run this against a real bucket; it needs credentials and a scratch bucket this environment does not have")
	}

	endpoint := requireEnv(t, "TRUSS_LEDGER_TEST_ENDPOINT")
	bucket := requireEnv(t, "TRUSS_LEDGER_TEST_RETENTION_BUCKET")
	region := requireEnv(t, "TRUSS_LEDGER_TEST_REGION")
	accessKeyID := requireEnv(t, "TRUSS_LEDGER_TEST_ACCESS_KEY_ID")
	secretAccessKey := requireEnv(t, "TRUSS_LEDGER_TEST_SECRET_ACCESS_KEY")
	prefix := requireEnv(t, "TRUSS_LEDGER_TEST_PREFIX")

	store, err := New(Config{
		Endpoint:        endpoint,
		Bucket:          bucket,
		Region:          region,
		Addressing:      PathStyle,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	key := prefix + "retention-" + randomHex(t)

	// The first write must succeed: retention forbids REPLACING an object
	// younger than the period, never creating one. A failure here is a
	// broken fixture (wrong bucket, wrong credential), not an answer.
	if err := store.Put(ctx, key, []byte("first")); err != nil {
		t.Fatalf("first Put to %s failed; retention should never block a create: %v", key, err)
	}

	// The second write is the measurement. Under an enforced retention
	// policy this must be refused, because the object is far younger than
	// the period.
	err = store.Put(ctx, key, []byte("second"))
	if err == nil {
		// Read it back rather than trusting the status code: a 2xx that
		// did not actually replace anything would be the "accepted and
		// then ignored" shape If-None-Match already turned out to have,
		// and it is the outcome most likely to be misread as success.
		got, getErr := store.Get(ctx, key)
		if getErr != nil {
			t.Fatalf("second Put reported success and the object could not be read back: %v", getErr)
		}
		t.Fatalf("RETENTION IS NOT ENFORCED ON THIS PATH: a second Put to %s succeeded and the object now reads %q. "+
			"A locked policy would therefore NOT protect this ledger against a SigV4 client, and the WORM design in "+
			"docs/work-items.md does not hold. Record this here before changing anything else.", key, string(got))
	}

	// An error is only the right answer if it is the RETENTION error. A
	// 403 for a missing permission, or a 404 for the wrong bucket, would
	// otherwise read as a pass and prove nothing -- the same shape as a
	// gate that reports success because its evidence is absent.
	if !strings.Contains(err.Error(), "retention") && !strings.Contains(err.Error(), "412") {
		t.Fatalf("second Put failed, but not with a retention refusal, so this measures nothing: %v", err)
	}

	t.Logf("retention IS enforced against SigV4 interop writes: %v", err)
}
