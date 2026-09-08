// The `publish` subcommand runs in the publisher container of the applier
// pod, never the truss container (design §2, §6). It holds the only Vault
// identity in the pod that can write -- a role bound to the audience
// "vault-publish", reachable only through a token the kubelet projects
// into this container and no other -- and it exists to keep that identity
// out of the container that runs OpenTofu. It also holds a second,
// independent credential: a 1Password service account scoped read-only to
// the `platform` vault, delivered as its own Kubernetes Secret mounted only
// into this container, which it uses to fetch the Cloudflare token itself
// rather than receive it over the handoff socket -- truss never sees the
// value, not in memory, not in a request, not in a log.
//
// It is a rendezvous, not a server: it loads its config and the authored
// expiry table, listens on one Unix socket for exactly one request
// (internal/handoff.Serve), answers it, and exits. No timer, no state read,
// no Vault login and no 1Password read until a request has actually
// arrived.
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
//
// ⚠️ OP_TOKEN_FILE AND THE 1PASSWORD VAULT NAME ARE DELIBERATELY NOT HERE.
// See loadPublishConfig's own doc for why: they gate a value publish, not
// startup.
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
// accept-side wait), so it derives its own bounded one. It also bounds the
// 1Password reads made on the same request, for the same reason.
const vaultCallTimeout = 60 * time.Second

// publishConfig is everything cmdPublish needs, read once from the
// environment with every problem reported before any of it is used --
// matching config.Load's and loadVaultConfig's own fail-closed contract.
type publishConfig struct {
	vault        secrets.KVConfig
	socket       string
	expiriesFile string
	wait         time.Duration
	// op is read but NOT validated here -- see loadPublishConfig's doc.
	op secrets.OPConfig
}

// loadPublishConfig reads and validates the publisher container's
// environment. EXPIRIES_FILE points at the read-only ConfigMap mount
// (design §4); it is never the repo checkout truss writes, and this
// function has no path by which it could become one -- there is no
// fallback to any other location.
//
// ⚠️ $OP_TOKEN_FILE AND THE 1PASSWORD VAULT NAME ARE READ HERE BUT NEVER
// VALIDATED HERE, AND THAT ASYMMETRY WITH EVERYTHING ELSE IN THIS FUNCTION
// IS DELIBERATE. Patching the authored expiry table -- this publisher's
// other, already-live job -- needs no 1Password at all, and this container
// is a native sidecar: if an absent or unreadable 1Password credential
// refused to let it start, the pod would sit Pending and, under
// concurrencyPolicy: Forbid, silently suppress every pass after it, over a
// credential that pass might never even need. So these two are optional at
// load. They become required, loudly, only at the moment handle() actually
// attempts to fetch a value -- see its own doc -- and even then there is no
// fallback: not to another token, not to an environment variable, not to
// the truss container's own `op-applier` mount, which carries broader
// access than this container is meant to have.
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
		op: secrets.OPConfig{
			Vault:     getenv("OP_VAULT"),
			TokenFile: getenv("OP_TOKEN_FILE"),
		},
	}, nil
}

// opReader is the seam handle lets a test double the 1Password reads
// without a real `op` process: the credential value (Field) and its
// expiry (Expiry), both against the compiled-in item. *secrets.OP is the
// real implementation.
type opReader interface {
	Field(ctx context.Context, item, field string) (string, error)
	Expiry(ctx context.Context, item string) (raw string, recorded bool, err error)
}

// newOPStore wraps secrets.NewOP so its return type matches opReader --
// production's only implementation of the seam. It is called from inside
// handle(), never from cmdPublish, which is what makes $OP_TOKEN_FILE and
// the 1Password vault name optional at startup and required only at the
// point of use (see loadPublishConfig's doc).
func newOPStore(cfg secrets.OPConfig) (opReader, error) {
	return secrets.NewOP(cfg)
}

// publishHandler closes over everything one request needs to answer: the
// write identity, a way to read the cas version PutValue's guard requires,
// the authored expiry table loaded once at startup, before any request has
// arrived, and how to build the 1Password reader a value publish needs
// (newOP, not a constructed opReader -- see handle's own doc for why
// construction is deferred rather than done once up front).
type publishHandler struct {
	publisher secrets.Publisher
	versions  currentVersionReader
	table     secrets.Expiries
	item      string // the one item this handler may write; itemCFInfraAdmin in production
	field     string // the field read from item for its value; fieldCFPassword in production
	opCfg     secrets.OPConfig
	newOP     func(secrets.OPConfig) (opReader, error) // newOPStore in production
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
// failure) -- and then, only when PublishValue is true, fetches the value
// and its expiry from 1Password and writes both.
//
// The 1Password reader is constructed HERE, not by cmdPublish before
// Serve, and only on this branch: loadPublishConfig deliberately leaves
// $OP_TOKEN_FILE and the vault name unvalidated, because a pass with
// nothing to publish (the common case) must not need them at all. An
// absent, empty or unreadable token surfaces here, in the Response, as
// this request's own failure -- loud, and never a fallback to any other
// credential.
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

	resp.Expiries = patched
	resp.Skipped = skipped

	if !req.PublishValue {
		resp.Value = handoff.ValueSkipped
		if firstErr != nil {
			resp.Error = firstErr.Error()
		}
		return finish(resp)
	}

	op, err := h.newOP(h.opCfg)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}

	// h.item and h.field are the compiled-in constants (itemCFInfraAdmin,
	// fieldCFPassword in production) -- never named by the request, which
	// carries only the PublishValue bool. PutValue below refuses any item
	// but the one it was constructed for regardless; this is the first,
	// independent refusal, not the only one.
	value, err := op.Field(ctx, h.item, h.field)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}
	expires, recorded, err := op.Expiry(ctx, h.item)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}
	if !recorded {
		// Op.Field already refuses a present-but-empty credential; this is
		// its counterpart for the expiry -- a fetch that finds nothing
		// recorded is an error here, never a silent skip (unlike Sweep's
		// walk over arbitrary items, where most legitimately carry none).
		resp.Value = handoff.ValueFailed
		resp.Error = fmt.Sprintf("refusing to publish: %q has no expiry recorded in 1Password", h.item)
		return finish(resp)
	}

	// The minted item's own expiry, like every table entry above, is a
	// best-effort PATCH: its failure is reported (note) alongside whatever
	// else failed, but must not stop the value write below from being
	// attempted -- design §9's "a value publish can succeed even when an
	// earlier expiry patch did not".
	if err := h.publisher.PatchExpiry(ctx, h.item, expires); err != nil {
		note(h.item+" (own expiry)", err)
	} else {
		patched++
	}
	resp.Expiries = patched
	resp.Skipped = skipped

	version, exists, err := h.versions.CurrentVersion(ctx, h.item)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}
	cas := 0
	if exists {
		cas = version
	}

	if err := h.publisher.PutValue(ctx, h.item, map[string]string{h.field: value}, cas); err != nil {
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
		field:     fieldCFPassword,
		opCfg:     cfg.op,
		newOP:     newOPStore,
	}

	if err := handoff.Serve(ctx, cfg.socket, cfg.wait, handler.handle); err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}
	return 0
}
