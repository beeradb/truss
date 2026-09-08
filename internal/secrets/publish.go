package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Publisher writes what the sweep reads. Deliberately separate from Store:
// Store's contract is "metadata only, it cannot return a secret value", and
// nothing here weakens that -- Publisher never reads a value either, only
// writes one. A caller that only needs to sweep never sees a Publisher; a
// caller that publishes gets one alongside, never instead of, a Store.
type Publisher interface {
	// PatchExpiry sets custom_metadata.expires on one item. It cannot
	// create an item and cannot touch any version's data.
	PatchExpiry(ctx context.Context, item, expires string) error
	// PutValue writes one item's fields under a check-and-set guard.
	// Refuses any item but the one this Publisher was constructed for.
	PutValue(ctx context.Context, item string, fields map[string]string, cas int) error
}

// PatchExpiry sets custom_metadata.expires on item via a JSON merge patch.
//
// It is PATCH, never POST, and that is load-bearing rather than stylistic:
// KV v2's metadata endpoint treats POST as a full write of the item's
// metadata *configuration* -- max_versions, cas_required and
// delete_version_after all reset to their defaults, silently, alongside
// whatever custom_metadata a POST happened to carry. PATCH with
// application/merge-patch+json changes only the keys named in the body.
// PATCH also cannot create an item: metadata for a title nobody has ever
// written a version of doesn't exist yet for PATCH to modify, so a typo'd
// item name 404s instead of inventing a credential slot for it to later be
// mistaken for.
//
// ⚠️ UNVERIFIED: whether the deployed Vault's KV v2 mount actually grants
// the `patch` capability and runs a plugin new enough to answer PATCH at
// all (design §13.2) -- the applier token available while this was written
// held only read on this mount ([read] on platform/data/*, [list read] on
// platform/metadata/*; no patch, no update, no create), so PATCH could not
// be exercised against the live server either while this was designed or
// while it was implemented. No read-modify-write-over-POST fallback is
// built for that case, deliberately: it would need the `update` capability
// this grant does not have either, and papering over a missing capability
// with a different write is exactly the kind of guess this project
// refuses. If the server rejects PATCH, patchFailureReason below turns the
// status into a message that does not conflate a missing policy grant
// (403 -- expected today, since that grant is a separate pending owner
// decision) with a KV plugin too old to support merge-patch at all (405) --
// those need different fixes from different people.
func (k *KV) PatchExpiry(ctx context.Context, item, expires string) error {
	if err := k.login(ctx); err != nil {
		return err
	}

	body, err := json.Marshal(struct {
		CustomMetadata map[string]string `json:"custom_metadata"`
	}{CustomMetadata: map[string]string{"expires": expires}})
	if err != nil {
		return fmt.Errorf("secrets: encoding the expiry patch for %q: %w", item, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, k.cfg.Addr+"/v1/"+k.cfg.Mount+"/metadata/"+item, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("secrets: building the expiry patch request for %q: %w", item, err)
	}
	req.Header.Set("Content-Type", "application/merge-patch+json")
	req.Header.Set("X-Vault-Token", k.token)

	resp, err := k.http.Do(req)
	if err != nil {
		return fmt.Errorf("secrets: patching the expiry of %q in %s: %s", item, k.cfg.Mount, k.redact(err.Error()))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("secrets: patching the expiry of %q in %s: %s", item, k.cfg.Mount, patchFailureReason(resp.StatusCode, k.redact(string(respBody))))
	}
	return nil
}

// patchFailureReason turns a failed PATCH's status into a message that
// points at the right person rather than a generic "PATCH failed" that
// could send anyone to check anything.
//
// A 403 and a 405 are NOT the same problem and must not read as one. A 403
// means the applier's Vault POLICY does not grant the `patch` capability on
// this path -- expected today, since that grant does not exist yet and is
// pending a separate owner decision (design §13.2, §8's new capability). A
// 405 (or a plugin old enough to not recognise the method at all) means the
// KV PLUGIN itself cannot answer PATCH, which no policy change fixes. Fixing
// the wrong one wastes whoever reads this message's time, so where the
// status genuinely cannot distinguish the two -- most non-2xx statuses,
// since this was written against no live server that could exercise either
// failure (§13 item 2) -- the message says exactly that instead of guessing.
//
// No read-modify-write-over-POST fallback exists for either case,
// deliberately: it would need the `update` capability this grant does not
// have either, and a POST to this endpoint resets the item's
// max_versions/cas_required/delete_version_after -- papering over a missing
// capability with a different, more damaging write is exactly the kind of
// guess this project refuses.
func patchFailureReason(status int, body string) string {
	switch status {
	case http.StatusForbidden:
		return fmt.Sprintf("vault returned 403 to the PATCH -- most likely the applier's Vault policy does not (yet) grant the \"patch\" capability on this path; that grant is a separate, pending owner decision, not a KV plugin problem: %s", body)
	case http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return fmt.Sprintf("vault returned %d to the PATCH -- this looks like the KV plugin on this mount does not support merge-patch at all (too old a version), not a policy problem: %s", status, body)
	default:
		return fmt.Sprintf("vault returned %d to the PATCH -- this could be a missing \"patch\" grant on the applier policy OR a KV plugin too old to support merge-patch, and the status alone does not say which; check both the policy grant and the KV plugin version before assuming either: %s", status, body)
	}
}

// PutValue writes item's fields as one new KV v2 version, guarded by cas --
// the version PutValue's caller last read, so a concurrent writer landing
// in between is detected. A cas mismatch is Vault's own signal that
// something else wrote first; it is returned as an error and PutValue never
// retries. Retrying would mean silently overwriting whatever that other
// writer just put there, which is the exact clobber cas exists to prevent,
// and this project has a standing rule against fallbacks and retry loops
// papering over a decision that belongs to a human.
//
// PutValue refuses any item other than the one this KV was constructed to
// write (KVConfig.WritableItem), and refuses an empty or whitespace-only
// value in any field before making any request at all -- an empty write
// would only be caught on the *next* sweep, by Dir.Field's own "is empty"
// refusal, and by then the damage (every non-credentials root failing to
// apply) is already done.
func (k *KV) PutValue(ctx context.Context, item string, fields map[string]string, cas int) error {
	if k.cfg.WritableItem == "" || item != k.cfg.WritableItem {
		return fmt.Errorf("secrets: refusing to write %q: this KV was constructed to write only %q", item, k.cfg.WritableItem)
	}
	if len(fields) == 0 {
		return fmt.Errorf("secrets: refusing to write %q: no fields given", item)
	}
	values := make([]string, 0, len(fields))
	for field, value := range fields {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("secrets: refusing to write %q.%q: value is empty or whitespace-only", item, field)
		}
		values = append(values, value)
	}

	if err := k.login(ctx); err != nil {
		return err
	}

	body, err := json.Marshal(struct {
		Options struct {
			CAS int `json:"cas"`
		} `json:"options"`
		Data map[string]string `json:"data"`
	}{
		Options: struct {
			CAS int `json:"cas"`
		}{CAS: cas},
		Data: fields,
	})
	if err != nil {
		return fmt.Errorf("secrets: encoding the value write for %q: %s", item, k.redact(err.Error(), values...))
	}

	// Over HTTP as a request body, never argv: this speaks Vault's HTTP
	// API directly and never shells out to a `vault` binary at all, so
	// there is no `ps`, `/proc` or exec-audit-log exposure to guard
	// against here by construction (design §6, and
	// vault/bootstrap-vault.sh's own reasoning for refusing argv).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.cfg.Addr+"/v1/"+k.cfg.Mount+"/data/"+item, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("secrets: building the value write request for %q: %s", item, k.redact(err.Error(), values...))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Vault-Token", k.token)

	resp, err := k.http.Do(req)
	if err != nil {
		return fmt.Errorf("secrets: writing the value of %q in %s: %s", item, k.cfg.Mount, k.redact(err.Error(), values...))
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("secrets: writing the value of %q in %s: vault returned %d (a cas mismatch means a concurrent writer; this is never retried): %s",
			item, k.cfg.Mount, resp.StatusCode, k.redact(string(respBody), values...))
	}
	return nil
}
