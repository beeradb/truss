package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// trimReasonLimit is the byte budget for a reason that leaves the pod,
// wherever it leaves from -- ledger failed/, heartbeat and (eventually)
// alert text (§2 item 13).
const trimReasonLimit = 800

const trimReasonMarker = "\n... truncated; see the run's pod logs for the rest."

// TrimReason strips NUL bytes and holds the result to trimReasonLimit.
//
// ⚠️ THE LENGTH IS MEASURED ON THE TEXT AS RECEIVED, BEFORE THE NULs COME
// OUT, so the truncation marker is appended whenever the ORIGINAL exceeds
// 800 bytes even if stripping alone would have brought it under the limit.
// That is deliberate rather than an oversight: the marker's job is to tell a
// reader that what they are looking at is not the whole reason, and by the
// time NULs have been removed the text on screen is already not what the run
// produced. Measuring after the strip would silently promote a mangled
// reason to a complete one.
func TrimReason(s string) string {
	original := []byte(s)

	stripped := make([]byte, 0, len(original))
	for _, b := range original {
		if b != 0 {
			stripped = append(stripped, b)
		}
	}

	first := stripped
	if len(first) > trimReasonLimit {
		first = first[:trimReasonLimit]
	}

	if len(original) > trimReasonLimit {
		return string(first) + trimReasonMarker
	}
	return string(first)
}

// RootSummary is what one root's apply contributed to an applied/<sha>
// record: the number of resource changes in the plan that was applied.
// A nil ResourceChanges is what gets recorded when `tofu show -json` could
// not be parsed: it marshals as `{"resource_changes": null}`, because an
// unknown count is recorded as unknown, not as zero. Zero is a claim that
// the apply changed nothing, which is exactly what nobody knows here.
type RootSummary struct {
	ResourceChanges *int `json:"resource_changes"`
}

// Expiring names one hand-held credential nearing or past its expiry, as
// produced by the daily expiry sweep. Its shape is not specified
// further by §4.2; it rides inside Heartbeat.Expiring as opaque data this
// package only needs to marshal in the right position.
type Expiring struct {
	Name string `json:"name"`
	// DaysLeft is whole days until expiry, negative when already past, and
	// nil when the credential records no expiry at all.
	//
	// ⚠️ THIS USED TO BE `Expires string` AND IT CHANGED THE HEARTBEAT'S
	// SCHEMA. The heartbeat's published shape is
	// {"name":…,"days_left":<number|null>}; what was written was
	// {"name":…,"expires":"in 5d"} -- the field renamed AND the number
	// stringified into prose. Any consumer of the heartbeat breaks on that,
	// and a heartbeat exists to be read by something outside this process,
	// so its schema is not ours to change quietly. It also makes two
	// heartbeats undiffable, which is the cheapest check anyone has that a
	// change to this pass did what it said. Found by the 2026-09-08 code
	// audit. The two packages' Expiring types now agree, so the crossing
	// point in cmd/truss is a copy rather than a reformat.
	DaysLeft *int `json:"days_left"`
}

// Heartbeat is written on every pass, success or failure, because a job
// that speaks only when it fails cannot be told apart from one that has
// stopped running. Field order is fixed -- time, last_sha, applied, noop,
// failure, rotation, drift, expiring -- because the heartbeat is read by
// eye as often as by machine, and a field that moves between passes makes
// two of them hard to compare. Go's encoding/json marshals struct fields in
// declaration order, so that order is this order.
type Heartbeat struct {
	Time     string          `json:"time"`
	LastSHA  string          `json:"last_sha"`
	Applied  int             `json:"applied"`
	Noop     int             `json:"noop"`
	Failure  *string         `json:"failure"`
	Rotation json.RawMessage `json:"rotation"`
	Drift    json.RawMessage `json:"drift"`
	Expiring []Expiring      `json:"expiring"`
}

// appliedRecord is the body written for a non-noop applied/<sha>: {"roots":
// {...}}. encoding/json sorts a map's string keys when marshaling, so the
// record is byte-identical for the same set of roots without any extra
// sorting code here -- and that determinism is deliberate (§3.4), because
// two applied records for the same commit have to be comparable.
type appliedRecord struct {
	Roots map[string]RootSummary `json:"roots"`
}

// failedRecord is the body written for failed/<sha>: reason then at, in
// that order, and the order is part of the record's contract rather than an
// accident of declaration.
type failedRecord struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// noopBody is written verbatim for a commit that touches no root -- no
// trailing newline, no other keys. A commit that did nothing still gets a
// record, so "applied and changed nothing" is distinguishable from "never
// reached".
const noopBody = `{"noop":true}`

// failedAtLayout is UTC, second resolution, Z-suffixed -- the one timestamp
// format every record in the ledger is written in.
const failedAtLayout = "2006-01-02T15:04:05Z"

// Journal is the ledger's write-and-read surface for the applier's own
// bookkeeping, over a Store and a Layout. Now defaults to time.Now when
// nil, matching Store's own default clock.
type Journal struct {
	Store  *Store
	Layout Layout
	Now    func() time.Time
}

func (j *Journal) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

// Head reads the commit HEAD was last advanced to. An absent HEAD is
// returned as ErrNotFound -- Journal does not guess one, because guessing
// means replaying history from nothing and applying every commit in the
// repository. The refusal that follows is the caller's job, not this
// method's.
func (j *Journal) Head(ctx context.Context) (string, error) {
	b, err := j.Store.Get(ctx, j.Layout.HeadKey)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// AdvanceHead writes sha as the new HEAD.
func (j *Journal) AdvanceHead(ctx context.Context, sha string) error {
	return j.Store.Put(ctx, j.Layout.HeadKey, []byte(sha))
}

// PutApplied records that sha applied, with roots keyed by root name.
func (j *Journal) PutApplied(ctx context.Context, sha string, roots map[string]RootSummary) error {
	body, err := json.Marshal(appliedRecord{Roots: roots})
	if err != nil {
		return fmt.Errorf("ledger: encoding applied record for %s: %w", sha, err)
	}
	return j.Store.Put(ctx, j.Layout.AppliedKey(sha), body)
}

// PutNoop records that sha touched no root: exactly {"noop":true}.
func (j *Journal) PutNoop(ctx context.Context, sha string) error {
	return j.Store.Put(ctx, j.Layout.AppliedKey(sha), []byte(noopBody))
}

// PutFailed records why sha failed. The reason is trimmed here,
// unconditionally -- callers do not have to remember to trim, because an
// untrimmed reason is refused at the one place every reason has to pass
// through on its way into the bucket (§4.2 "Refuses to. ... Write an
// untrimmed reason.").
func (j *Journal) PutFailed(ctx context.Context, sha, reason string) error {
	rec := failedRecord{
		Reason: TrimReason(reason),
		At:     j.now().UTC().Format(failedAtLayout),
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ledger: encoding failed record for %s: %w", sha, err)
	}
	return j.Store.Put(ctx, j.Layout.FailedKey(sha), body)
}

// PutHeartbeat writes hb to the heartbeat key. hb.Failure is trimmed here
// too, and independently of PutFailed rather than relying on it: the
// heartbeat lands in the same world-readable bucket the ledger does, and it
// is written on passes where PutFailed never runs at all.
func (j *Journal) PutHeartbeat(ctx context.Context, hb Heartbeat) error {
	if hb.Failure != nil {
		trimmed := TrimReason(*hb.Failure)
		hb.Failure = &trimmed
	}
	body, err := json.Marshal(hb)
	if err != nil {
		return fmt.Errorf("ledger: encoding heartbeat: %w", err)
	}
	return j.Store.Put(ctx, j.Layout.HeartbeatKey, body)
}

// ApprovedDigest reads the plan digest CI recorded for root at headSHA. A
// missing digest surfaces as ErrNotFound, which means "no approved plan was
// ever recorded" and must end as a refusal to apply a plan nobody reviewed
// -- but that decision is the caller's to make, not this method's.
func (j *Journal) ApprovedDigest(ctx context.Context, headSHA, root string) (string, error) {
	b, err := j.Store.Get(ctx, j.Layout.DigestKey(headSHA, root))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
