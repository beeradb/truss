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
// ⚠️ IT USED TO BE A RENDEZVOUS, NOT A SERVER: load config and the
// authored expiry table once, serve exactly one request, exit. Under a
// long-running applier that runs one drift pass a day, that shape leaves
// 364 days of the year with nobody listening. It is a server now:
// internal/handoff.Serve answers requests until told to stop, and every
// per-request-shaped assumption below (the Vault write identity, the
// expiry table) is rebuilt fresh on every request rather than held from
// startup -- see publishHandler's own doc for why each specifically must
// be.
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

	// ⚠️ $PUBLISH_WAIT RETIRES, AND ITS PRESENCE IS A REFUSAL RATHER THAN A
	// SILENT NO-OP. It bounded the old rendezvous's accept-side wait, whose
	// job (design §3's "truss died without ever asking") now belongs to
	// the loop's own heartbeat staleness. A manifest that still sets it is
	// an operator who believes a timeout exists here; silently ignoring
	// the variable would leave that belief uncorrected.
	if getenv("PUBLISH_WAIT") != "" {
		problems = append(problems, "refusing to start: $PUBLISH_WAIT is set but no longer read -- the publisher is a long-lived server now and has no accept-side timeout; remove it from the manifest")
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

// publishHandler closes over everything a request needs, but holds only
// FACTORIES and PATHS for the two things that must be fresh on every
// call, never a constructed value:
//
//   - newVault, because secrets.KV caches its Vault token for its own
//     lifetime and never refreshes it (internal/secrets/kv.go). A
//     publisher that built one at startup would work until the token's
//     TTL expired and then 403 every request for as long as the pod
//     lived -- once a day, on the one pass that rotates, with nobody
//     watching. It also re-reads the projected ServiceAccount JWT from
//     disk, which the kubelet rotates underneath this process.
//   - expiriesFile, the PATH rather than the loaded table, because it is
//     a ConfigMap mount that Kubernetes updates IN PLACE: holding the
//     table in memory from startup would mean an edited table never
//     takes effect for as long as the pod lived.
//
// newOP is the same shape for the 1Password reader, and predates this
// change -- see handle's own doc for why its construction is already
// deferred to the point of use.
type publishHandler struct {
	newVault     func(secrets.KVConfig) (vaultWriter, error) // newVaultWriter in production
	vaultCfg     secrets.KVConfig
	expiriesFile string
	item         string // the one item this handler may write; itemCFInfraAdmin in production
	field        string // the field read from item for its value; fieldCFPassword in production
	opCfg        secrets.OPConfig
	newOP        func(secrets.OPConfig) (opReader, error) // newOPStore in production
}

// currentVersionReader is the seam handle lets a test double the KV v2
// version lookup without a real Vault. *secrets.KV is the real
// implementation.
type currentVersionReader interface {
	CurrentVersion(ctx context.Context, item string) (version int, exists bool, err error)
}

// vaultWriter is the union of what one request needs from the Vault write
// identity: secrets.Publisher for the patch/write calls, plus the CAS
// version lookup PutValue's guard requires. *secrets.KV satisfies it by
// its method set alone -- one construction answers both.
type vaultWriter interface {
	secrets.Publisher
	currentVersionReader
}

// newVaultWriter is production's only implementation of h.newVault.
func newVaultWriter(cfg secrets.KVConfig) (vaultWriter, error) {
	return secrets.NewKV(cfg)
}

// loadExpiryTable re-reads the authored expiry table from its ConfigMap
// mount. Called fresh on every request -- see publishHandler's own doc.
func loadExpiryTable(path string) (secrets.Expiries, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening the expiry table at %s: %w", path, err)
	}
	defer f.Close()
	return secrets.LoadExpiries(f, []string{itemCFTokenMint})
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

	// ⚠️ BOTH REBUILT HERE, ON EVERY REQUEST -- see publishHandler's own
	// doc for why. A construction failure is THIS REQUEST'S failure, the
	// same fail-at-the-point-of-use discipline loadPublishConfig's own doc
	// already argues for $OP_TOKEN_FILE: never a process exit, never a
	// fallback to a stale value held from an earlier request.
	publisher, err := h.newVault(h.vaultCfg)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = fmt.Sprintf("could not build the Vault write identity: %v", err)
		return finish(resp)
	}
	table, err := loadExpiryTable(h.expiriesFile)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}

	// The authored table (design §4): read-only ConfigMap, never the repo
	// checkout. Sorted so the order patches are attempted in -- and
	// therefore which name lands first in Skipped on a partial failure --
	// is deterministic rather than Go's randomised map order.
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := publisher.PatchExpiry(ctx, name, table[name]); err != nil {
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
	if err := publisher.PatchExpiry(ctx, h.item, expires); err != nil {
		note(h.item+" (own expiry)", err)
	} else {
		patched++
	}
	resp.Expiries = patched
	resp.Skipped = skipped

	version, exists, err := publisher.CurrentVersion(ctx, h.item)
	if err != nil {
		resp.Value = handoff.ValueFailed
		resp.Error = err.Error()
		return finish(resp)
	}
	cas := 0
	if exists {
		cas = version
	}

	if err := publisher.PutValue(ctx, h.item, map[string]string{h.field: value}, cas); err != nil {
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

// cmdPublish is `truss publish`: load config, sanity-check the expiry
// table and the Vault write identity, then serve handoff requests until
// ctx is cancelled. It takes no arguments.
//
// Exit 0 when Serve stops because ctx was cancelled (the loop's own
// shutdown), regardless of any individual request's own outcome -- a
// per-request Vault failure travels in that request's Response.Error,
// never in this process's exit code. Exit 1 covers every refusal before
// or during serving: bad config, an expiry table that will not even parse
// at boot, a Vault client that could not be constructed at boot, or the
// listener itself failing.
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

	// Boot-time sanity checks only -- neither value is kept. handle()
	// (below) rebuilds both fresh on every request, for the reasons
	// publishHandler's own doc states; this is only "does the mount exist
	// and parse, does Vault answer at all" failing fast and loudly rather
	// than waiting for the first real request a day from now to find out.
	if _, err := loadExpiryTable(cfg.expiriesFile); err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}
	if _, err := newVaultWriter(cfg.vault); err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}

	handler := publishHandler{
		newVault:     newVaultWriter,
		vaultCfg:     cfg.vault,
		expiriesFile: cfg.expiriesFile,
		item:         itemCFInfraAdmin,
		field:        fieldCFPassword,
		opCfg:        cfg.op,
		newOP:        newOPStore,
	}

	if err := handoff.Serve(ctx, cfg.socket, handler.handle); err != nil {
		fmt.Fprintf(stderr, "publish: %v\n", err)
		return 1
	}
	return 0
}
