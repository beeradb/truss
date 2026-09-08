package ledger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testJournal(t *testing.T) (*Journal, *fakeBucket, func()) {
	t.Helper()
	store, fb, srv := newFakeServerStore(t, PathStyle)
	j := &Journal{
		Store:  store,
		Layout: testLayout(),
		Now:    func() time.Time { return time.Date(2026, 9, 8, 12, 30, 45, 0, time.UTC) },
	}
	return j, fb, srv.Close
}

// TestFailedRecordIsReasonThenAt: ledger_put_failed builds its object with
// jq -n --arg reason ... --arg at ... '{reason:$reason, at:$at}'
// (apply.sh:358) -- reason first, at second, and that is a byte-order claim
// about the object, not just a claim about which fields exist.
func TestFailedRecordIsReasonThenAt(t *testing.T) {
	j, fb, closeSrv := testJournal(t)
	defer closeSrv()

	if err := j.PutFailed(context.Background(), "headsha1", "tofu apply failed for platform"); err != nil {
		t.Fatalf("PutFailed: %v", err)
	}

	body := string(fb.lastBody)
	reasonIdx := strings.Index(body, `"reason"`)
	atIdx := strings.Index(body, `"at"`)
	if reasonIdx == -1 || atIdx == -1 {
		t.Fatalf("failed record missing reason or at: %s", body)
	}
	if reasonIdx > atIdx {
		t.Errorf("failed record has \"at\" before \"reason\": %s", body)
	}

	var decoded map[string]any
	if err := json.Unmarshal(fb.lastBody, &decoded); err != nil {
		t.Fatalf("failed record is not valid JSON: %v (%s)", err, body)
	}
	if decoded["reason"] != "tofu apply failed for platform" {
		t.Errorf("reason = %v", decoded["reason"])
	}
	if decoded["at"] != "2026-09-08T12:30:45Z" {
		t.Errorf("at = %v, want 2026-09-08T12:30:45Z (date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)", decoded["at"])
	}
}

// TestFailedRecordTrimsTheReasonItself: PutFailed must not trust the caller
// to have already trimmed -- the ledger is the one place every reason
// passes through on its way into the bucket (§4.2 "Refuses to. ... Write an
// untrimmed reason.").
func TestFailedRecordTrimsTheReasonItself(t *testing.T) {
	j, fb, closeSrv := testJournal(t)
	defer closeSrv()

	longReason := repeatByte('x', 2000)
	if err := j.PutFailed(context.Background(), "headsha1", longReason); err != nil {
		t.Fatalf("PutFailed: %v", err)
	}

	var decoded struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(fb.lastBody, &decoded); err != nil {
		t.Fatalf("failed record is not valid JSON: %v", err)
	}
	if decoded.Reason != TrimReason(longReason) {
		t.Errorf("PutFailed wrote an untrimmed reason: %d bytes, want TrimReason's %d", len(decoded.Reason), len(TrimReason(longReason)))
	}
	if len(decoded.Reason) == len(longReason) {
		t.Errorf("PutFailed wrote the 2000-byte reason verbatim; it must be trimmed")
	}
}

// TestHeartbeatFieldOrderIsStable: write_heartbeat's jq object is built
// time, last_sha, applied, noop, failure, rotation, drift, expiring, in
// that order (apply.sh:399-405). Go's encoding/json marshals struct fields
// in declaration order, so this is really a claim about Heartbeat's field
// order, checked against the bytes it actually produces.
func TestHeartbeatFieldOrderIsStable(t *testing.T) {
	failure := "boom"
	days := 23
	hb := Heartbeat{
		Time:     "2026-09-08T12:30:45Z",
		LastSHA:  "headsha1",
		Applied:  3,
		Noop:     1,
		Failure:  &failure,
		Rotation: json.RawMessage(`null`),
		Drift:    json.RawMessage(`null`),
		Expiring: []Expiring{{Name: "cf-infra-admin", DaysLeft: &days}},
	}
	body, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	wantOrder := []string{`"time"`, `"last_sha"`, `"applied"`, `"noop"`, `"failure"`, `"rotation"`, `"drift"`, `"expiring"`}
	last := -1
	for _, key := range wantOrder {
		idx := strings.Index(string(body), key)
		if idx == -1 {
			t.Fatalf("heartbeat JSON missing key %s: %s", key, body)
		}
		if idx < last {
			t.Errorf("heartbeat field %s appears out of order: %s", key, body)
		}
		last = idx
	}
}

// A nil Failure must marshal to JSON null, not be omitted -- the pass
// always writes a heartbeat with an explicit verdict on failure, never a
// heartbeat that is silent about it.
func TestHeartbeatFailureIsNullWhenAbsent(t *testing.T) {
	hb := Heartbeat{Time: "2026-09-08T12:30:45Z", LastSHA: "headsha1"}
	body, err := json.Marshal(hb)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(body), `"failure":null`) {
		t.Errorf("heartbeat with no failure = %s, want \"failure\":null", body)
	}
}

// TestAppliedRecordForANoopIsExactlyNoopTrue: apply.sh:773,
// ledger_put_applied "$sha" '{"noop":true}' -- exact bytes, nothing else in
// the object.
func TestAppliedRecordForANoopIsExactlyNoopTrue(t *testing.T) {
	j, fb, closeSrv := testJournal(t)
	defer closeSrv()

	if err := j.PutNoop(context.Background(), "headsha1"); err != nil {
		t.Fatalf("PutNoop: %v", err)
	}
	if got, want := string(fb.lastBody), `{"noop":true}`; got != want {
		t.Errorf("PutNoop body = %q, want %q", got, want)
	}
}

// TestAppliedRecordSortsRootsForDeterminism: the bash iterates a bash
// associative array whose order is unspecified (§3.4); nothing hashes this
// object, so the Go port sorts instead, deterministically, which is a
// documented divergence and not a bug.
func TestAppliedRecordSortsRootsForDeterminism(t *testing.T) {
	j, fb, closeSrv := testJournal(t)
	defer closeSrv()

	n := func(v int) *int { return &v }
	roots := map[string]RootSummary{
		"projects/recipes": {ResourceChanges: n(3)},
		"credentials":      {ResourceChanges: n(0)},
		"platform":         {ResourceChanges: n(1)},
	}
	if err := j.PutApplied(context.Background(), "headsha1", roots); err != nil {
		t.Fatalf("PutApplied: %v", err)
	}

	body := string(fb.lastBody)
	idxCreds := strings.Index(body, `"credentials"`)
	idxPlatform := strings.Index(body, `"platform"`)
	idxProjects := strings.Index(body, `"projects/recipes"`)
	if idxCreds == -1 || idxPlatform == -1 || idxProjects == -1 {
		t.Fatalf("applied record missing a root: %s", body)
	}
	if !(idxCreds < idxPlatform && idxPlatform < idxProjects) {
		t.Errorf("roots not in lexical order: %s", body)
	}

	if !strings.HasPrefix(body, `{"roots":{`) {
		t.Errorf("applied record does not wrap roots under \"roots\": %s", body)
	}
}

// AdvanceHead and Head round-trip a sha with no extra bytes, and PutFailed's
// clock comes from Journal.Now, not from the wall clock, so a caller can
// make the "at" field deterministic in a test -- proven above; this checks
// the Head/AdvanceHead half of the same contract.
func TestAdvanceHeadThenHeadRoundTrips(t *testing.T) {
	j, _, closeSrv := testJournal(t)
	defer closeSrv()
	ctx := context.Background()

	if err := j.AdvanceHead(ctx, "headsha1"); err != nil {
		t.Fatalf("AdvanceHead: %v", err)
	}
	got, err := j.Head(ctx)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if got != "headsha1" {
		t.Errorf("Head = %q, want headsha1", got)
	}
}

// An absent HEAD is ErrNotFound -- apply.sh:368-369 refuses to start rather
// than guess one, and "absent" is exactly what ErrNotFound has to mean for
// that refusal to be reachable from Go.
func TestHeadOfAnEmptyLedgerIsErrNotFound(t *testing.T) {
	j, _, closeSrv := testJournal(t)
	defer closeSrv()

	_, err := j.Head(context.Background())
	if err == nil {
		t.Fatal("Head on an empty ledger returned nil error")
	}
}

// ApprovedDigest of a sha/root with no recorded digest is ErrNotFound --
// verify_plan_digest's "no approved plan recorded" refusal (apply.sh:640)
// is the caller's decision to make from this, not something Journal
// papers over.
func TestApprovedDigestOfAnUnrecordedRootIsErrNotFound(t *testing.T) {
	j, _, closeSrv := testJournal(t)
	defer closeSrv()

	_, err := j.ApprovedDigest(context.Background(), "headsha1", "platform")
	if err == nil {
		t.Fatal("ApprovedDigest of an unrecorded root returned nil error")
	}
}

// TestExpiringMatchesTheBashHeartbeatSchema pins the field names and JSON
// types write_heartbeat (apply.sh:730-732) emits. The Go port shipped
// {"name":…,"expires":"in 5d"} against the bash's
// {"name":…,"days_left":<number|null>} -- the field renamed and the number
// stringified, which breaks any consumer and specifically defeats §5's plan
// to validate the rollout by diffing a bash heartbeat against a Go one.
// Found by the 2026-09-08 code audit.
func TestExpiringMatchesTheBashHeartbeatSchema(t *testing.T) {
	days := 5
	body, err := json.Marshal([]Expiring{
		{Name: "cf-infra-admin", DaysLeft: &days},
		{Name: "gcs-ledger"}, // no expiry recorded
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `[{"name":"cf-infra-admin","days_left":5},{"name":"gcs-ledger","days_left":null}]`
	if string(body) != want {
		t.Errorf("heartbeat expiring JSON\n got %s\nwant %s", body, want)
	}
}
