// The `publish` subcommand runs in the publisher container of the applier
// pod, never the truss container (design §2, §6). It holds the only Vault
// identity in the pod that can write -- a role bound to the audience
// "vault-publish", reachable only through a token the kubelet projects
// into this container and no other -- and it exists to keep that identity
// out of the container that runs OpenTofu.
//
// It is a rendezvous, not a server: it loads its config and the authored
// expiry table, listens on one Unix socket for exactly one request
// (internal/handoff.Serve), answers it, and exits. No timer, no state read,
// no Vault login until a request has actually arrived.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/beeradb/truss/internal/handoff"
	"github.com/beeradb/truss/internal/secrets"
)

// publishRequiredEnv is deliberately its own list, disjoint from
// vaultRequiredEnv's callers and from internal/config.Config: this
// subcommand runs in a container that shares no configuration path with
// the applier's config.Load or cmd/truss/apply_cmd.go, which is the whole
// point of a separate identity (design §2, §6) -- nothing on this side is
// reachable from the truss container, including by accident through a
// shared config loader.
var publishRequiredEnv = []string{
	"VAULT_ADDR", "VAULT_ROLE", "VAULT_JWT_PATH", "VAULT_MOUNT",
	"HANDOFF_SOCKET", "EXPIRIES_FILE",
}

// defaultPublishWait matches design §3's "PUBLISH_WAIT, default 30m": the
// publisher's wait is bounded, and its exit code carries meaning (a failed
// pass) only in the one case nobody ever connected.
const defaultPublishWait = 30 * time.Minute

// vaultCallTimeout bounds the handler's own Vault work per request. The
// handler receives no context from internal/handoff.Serve (h's signature
// is func(Request) Response, deliberately -- Serve's ctx governs only the
// accept-side wait), so it derives its own bounded one.
const vaultCallTimeout = 60 * time.Second

// publishConfig is everything cmdPublish needs, read once from the
// environment with every problem reported before any of it is used --
// matching config.Load's and loadVaultConfig's own fail-closed contract.
type publishConfig struct {
	vault        secrets.KVConfig
	socket       string
	expiriesFile string
	wait         time.Duration
}

// loadPublishConfig reads and validates the publisher container's
// environment. EXPIRIES_FILE points at the read-only ConfigMap mount
// (design §4); it is never the repo checkout truss writes, and this
// function has no path by which it could become one -- there is no
// fallback to any other location.
func loadPublishConfig(getenv func(string) string) (publishConfig, []string) {
	values := make(map[string]string, len(publishRequiredEnv))
	var problems []string
	for _, name := range publishRequiredEnv {
		v := getenv(name)
		if v == "" {
			problems = append(problems, fmt.Sprintf("refusing to start: $%s is unset", name))
		}
		values[name] = v
	}

	wait := defaultPublishWait
	if raw := getenv("PUBLISH_WAIT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			problems = append(problems, fmt.Sprintf("refusing to start: $PUBLISH_WAIT is not a valid duration: %v", err))
		} else {
			wait = d
		}
	}

	if len(problems) > 0 {
		return publishConfig{}, problems
	}

	return publishConfig{
		vault: secrets.KVConfig{
			Addr:         values["VAULT_ADDR"],
			Mount:        values["VAULT_MOUNT"],
			Role:         values["VAULT_ROLE"],
			JWTPath:      values["VAULT_JWT_PATH"],
			WritableItem: itemCFInfraAdmin,
		},
		socket:       values["HANDOFF_SOCKET"],
		expiriesFile: values["EXPIRIES_FILE"],
		wait:         wait,
	}, nil
}

// publishHandler closes over everything one request needs to answer: the
// write identity, a way to read the cas version PutValue's guard requires,
// and the authored expiry table loaded once at startup, before any request
// has arrived.
type publishHandler struct {
	publisher secrets.Publisher
	versions  currentVersionReader
	table     secrets.Expiries
	item      string // the one item this handler may write; itemCFInfraAdmin in production
}

// currentVersionReader is the seam handle lets a test double the KV v2
// version lookup without a real Vault. *secrets.KV is the real
// implementation.
type currentVersionReader interface {
	CurrentVersion(ctx context.Context, item string) (version int, exists bool, err error)
}

// handle answers one request. It always attempts the authored expiry
// table first, regardless of PublishValue -- a pass with nothing to
// publish still exists to keep the expiry alarm honest (design §9,
// "Nothing to publish; the expiry patches failed" is reported, not a
// failure) -- and then, only when PublishValue is true, writes the value
// and the minted item's own expiry.
//
// The vacuous-pass rule (design §9's last row) is enforced at the end:
// wrote nothing, skipped nothing, and had nothing to do is refused, never
// silently reported as a success.
func (h publishHandler) handle(req handoff.Request) handoff.Response {
	ctx, cancel := context.WithTimeout(context.Background(), vaultCallTimeout)
	defer cancel()

	var resp handoff.Response
	var skipped []string
	patched := 0
	var firstErr error

	note := func(name string, err error) {
		skipped = append(skipped, fmt.Sprintf("%s: %v", name, err))
		if firstErr == nil {
			firstErr = err
		}
	}

	// The authored table (design §4): read-only ConfigMap, never the repo
	// checkout, loaded once by cmdPublish before any request arrived.
	// Sorted so the order patches are attempted in -- and therefore which
	// name lands first in Skipped on a partial failure -- is deterministic
	// rather than Go's randomised map order.
	names := make([]string, 0, len(h.table))
	for name := range h.table {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := h.publisher.PatchExpiry(ctx, name, h.table[name]); err != nil {
			note(name, err)
			continue
		}
		patched++
	}

	// The minted item's own expiry travels in the request, because it is a
	// fact about this mint that only the mint knows (design §4, "one row").
	// It is never read from the authored table, and the table's own loader
	// (secrets.LoadExpiries) already refuses an entry naming a probed item
	// -- this is neither: it is the one row truss itself still controls.
	if req.PublishValue && req.Expires != "" {
		if err := h.publisher.PatchExpiry(ctx, req.Item, req.Expires); err != nil {
			note(req.Item+" (own expiry)", err)
		} else {
			patched++
		}
	}

	resp.Expiries = patched
	resp.Skipped = skipped

	if !req.PublishValue {
		resp.Value = handoff.ValueSkipped
		if firstErr != nil {
			resp.Error = firstErr.Error()
		}
		return finish(resp)
	}

	if req.Item != h.item {
		resp.Value = handoff.ValueFailed
		resp.Error = fmt.Sprintf("refusing to publish %q: this publisher may write only %q", req.Item, h.item)
		return finish(resp)
	}
	if req.Field == "" || req.Value == "" {
		resp.Value = handoff.ValueFailed
		resp.Error = "refusing to publish: field and value must both be set when publish_value is true"
		return finish(resp)
	}

	version, exists, err := h.versions.CurrentVersion(ctx, req.Item)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}
	cas := 0
	if exists {
		cas = version
	}

	if err := h.publisher.PutValue(ctx, req.Item, map[string]string{req.Field: req.Value}, cas); err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}

	resp.Value = handoff.ValueWritten
	// A value publish can succeed even when an earlier expiry patch did
	// not: design §9's "Some expiries written, then one failed" row --
	// both the findings (Expiries, Skipped) and the error travel together,
	// never one silently dropped in favour of the other.
	if firstErr != nil {
		resp.Error = firstErr.Error()
	}
	return finish(resp)
}

// finish enforces the vacuous-pass rule: a response that wrote nothing,
// skipped nothing (so nothing failed either) and has no error of its own
// is not a success -- it is a request this handler had nothing to do with,
// which must never be reported the same way as a clean pass (design §9's
// closing row, and the standing rule that a check degrading to a vacuous
// pass is worse than one that is absent).
func finish(resp handoff.Response) handoff.Response {
	if resp.Expiries == 0 && len(resp.Skipped) == 0 && resp.Value != handoff.ValueWritten {
		resp.Value = handoff.ValueFailed
		resp.Error = "publish: refusing to report success -- nothing was patched, nothing failed, and no value was published"
	}
	return resp
}

// cmdPublish is `truss publish`: load config, load the expiry table, serve
// exactly one handoff request, and exit. It takes no arguments.
//
// Exit 0 only when a request was served, regardless of whether that
// request's own outcome was a Vault failure -- the Response.Error is how a
// per-item failure reaches truss; cmdPublish's own exit code answers a
// narrower question, "did I serve the rendezvous I exist for", the same
// way internal/handoff.Serve's returned error does. Exit 1 covers every
// refusal before or during serving: bad config, an invalid expiry table, a
// Vault client that could not even be constructed, or nobody ever
// connecting before PUBLISH_WAIT elapsed.
func cmdPublish(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss publish")
		return 2
	}

	cfg, problems := loadPublishConfig(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	expiriesFile, err := os.Open(cfg.expiriesFile)
	if err != nil {
		fmt.Fprintf(stderr, "publish: opening the expiry table at %s: %v\n", cfg.expiriesFile, err)
		return 1
	}
	table, err := secrets.LoadExpiries(expiriesFile, []string{itemCFTokenMint})
	expiriesFile.Close()
	if err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}

	kv, err := secrets.NewKV(cfg.vault)
	if err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}

	handler := publishHandler{
		publisher: kv,
		versions:  kv,
		table:     table,
		item:      itemCFInfraAdmin,
	}

	if err := handoff.Serve(ctx, cfg.socket, cfg.wait, handler.handle); err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}
	return 0
}
