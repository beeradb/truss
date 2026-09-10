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

// TrimReason mirrors apply.sh's trim_reason (338-354) byte for byte,
// including its one quirk: the truncation marker is appended whenever the
// ORIGINAL text exceeds 800 bytes, even if stripping NUL bytes first would
// have brought it under the limit on its own. That is what the bash does --
// wc -c runs on $1 before tr -d strips anything -- and this is a faithful
// port, not a cleaner rewrite of it.
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
// A nil ResourceChanges matches summary_from_plan's fallback when `tofu
// show -json` could not be parsed (apply.sh:598, `{"resource_changes":
// null}`) -- an unknown count is recorded as unknown, not as zero.
type RootSummary struct {
	ResourceChanges *int `json:"resource_changes"`
}

// Expiring names one hand-held credential nearing or past its expiry, as
// produced by check_credential_lifetimes. Its shape is not specified
// further by §4.2; it rides inside Heartbeat.Expiring as opaque data this
// package only needs to marshal in the right position.
type Expiring struct {
	Name string `json:"name"`
	// DaysLeft is whole days until expiry, negative when already past, and
	// nil when the credential records no expiry at all.
	//
	// ⚠️ THIS USED TO BE `Expires string` AND IT CHANGED THE HEARTBEAT'S
	// SCHEMA. write_heartbeat (apply.sh:730-732) emits
	// {"name":…,"days_left":<number|null>}; the Go port emitted
	// {"name":…,"expires":"in 5d"} -- the field renamed AND the number
	// stringified. Any consumer of the heartbeat breaks on that, and
	// specifically it defeats port-plan §5's plan to validate the rollout by
	// diffing a bash heartbeat against a Go one. Found by the 2026-09-08
	// code audit. The two packages' Expiring types now agree, so the
	// crossing point in cmd/truss is a copy rather than a reformat.
	DaysLeft *int `json:"days_left"`
}

// Heartbeat is written on every pass, success or failure, because a job
// that speaks only when it fails cannot be told apart from one that has
// stopped running. Field order is fixed -- time, last_sha, applied, noop,
// failure, rotation, drift, expiring -- matching write_heartbeat
// (apply.sh:399-405); Go's encoding/json marshals struct fields in
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
// {...}}, matching apply.sh:841. encoding/json sorts a map's string keys
// when marshaling, so this is deterministic without any extra sorting code
// here -- the deliberate divergence from the bash's unspecified iteration
// order (§3.4).
type appliedRecord struct {
	Roots map[string]RootSummary `json:"roots"`
}

// failedRecord is the body written for failed/<sha>: reason then at, in
// that order, matching ledger_put_failed's jq object (apply.sh:358).
type failedRecord struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// noopBody is written verbatim for a commit that touches no root
// (apply.sh:773) -- no trailing newline, no other keys.
const noopBody = `{"noop":true}`

// skippedRecord is the body written for a commit skipped through `truss
// skip` (docs/work-items.md:86-133): skipped, reason, at, in that field
// order. It lives under the APPLIED prefix, not a prefix of its own --
// applied/<sha> already means "what happened to this commit in the
// queue", and `truss why` reads exactly that one key (falling back to
// failed/<sha>) to answer, so a second prefix would mean two keys to
// check for the same question. The `"skipped":true` tag is what stops a
// reader from mistaking this for appliedRecord's `{"roots":...}` shape.
//
// Deliberately no `by` field: recording an unverified $USER is theatre --
// nothing here authenticates it, and a recorded identity nobody checked is
// worse than an absent one, because it invites trust an unauthenticated
// string cannot earn.
type skippedRecord struct {
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason"`
	At      string `json:"at"`
}

// failedAtLayout is `date -u +%Y-%m-%dT%H:%M:%SZ` (apply.sh:358).
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
// returned as ErrNotFound -- Journal does not guess one; apply.sh:368-369
// refuses to start rather than replay history from nothing, and that
// refusal is the caller's job, not this method's.
func (j *Journal) Head(ctx context.Context) (string, error) {
	b, err := j.Store.Get(ctx, j.Layout.HeadKey)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// AdvanceHead writes sha as the new HEAD (apply.sh:360, advance_head).
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

// PutNoop records that sha touched no root: exactly {"noop":true}
// (apply.sh:773).
func (j *Journal) PutNoop(ctx context.Context, sha string) error {
	return j.Store.Put(ctx, j.Layout.AppliedKey(sha), []byte(noopBody))
}

// PutSkipped records that an operator skipped sha via `truss skip`,
// advancing HEAD past a commit the applier never applied. Unlike
// PutFailed's reason -- tofu's own error text, which can run to kilobytes
// -- reason here is typed by a person at a terminal for exactly this
// record, so there is nothing worth trimming.
func (j *Journal) PutSkipped(ctx context.Context, sha, reason string) error {
	rec := skippedRecord{
		Skipped: true,
		Reason:  reason,
		At:      j.now().UTC().Format(failedAtLayout),
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("ledger: encoding skipped record for %s: %w", sha, err)
	}
	return j.Store.Put(ctx, j.Layout.AppliedKey(sha), body)
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
// too, for the same reason PutFailed trims its reason: write_heartbeat
// trims independently of ledger_put_failed (apply.sh:398), because the
// heartbeat lands in the same world-readable bucket the ledger does.
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
// missing digest surfaces as ErrNotFound; verify_plan_digest
// (apply.sh:631-651) treats that as "refusing to apply a plan nobody
// reviewed", which is the caller's decision to make, not this method's.
func (j *Journal) ApprovedDigest(ctx context.Context, headSHA, root string) (string, error) {
	b, err := j.Store.Get(ctx, j.Layout.DigestKey(headSHA, root))
	if err != nil {
		return "", err
	}
	return string(b), nil
}
