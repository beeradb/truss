package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/beeradb/truss/internal/config"
	"github.com/beeradb/truss/internal/forge"
	"github.com/beeradb/truss/internal/gates"
	"github.com/beeradb/truss/internal/ledger"
	"github.com/beeradb/truss/internal/notify"
	"github.com/beeradb/truss/internal/plan"
	"github.com/beeradb/truss/internal/repo"
	"github.com/beeradb/truss/internal/secrets"
)

// tofuRunner is the subset of plan.Runner's methods the apply pass calls.
// plan.Runner satisfies this by its method set alone (Go's structural
// typing needs no explicit declaration); tests supply a fake instead of
// shelling out to a real `tofu` binary.
type tofuRunner interface {
	Init(ctx context.Context, dir string) error
	Plan(ctx context.Context, dir, outFile string) error
	PlanDetailed(ctx context.Context, dir string) (bool, error)
	Apply(ctx context.Context, dir, planFile string) error
	ShowJSON(ctx context.Context, dir, planFile string) ([]byte, error)
}

// tofuFactory builds a tofuRunner for one root apply, given that root's
// exact child environment (§2 item 9: Runner.Env is never inherited
// implicitly, so building it explicitly, per call, is this factory's job).
type tofuFactory func(env []string) tofuRunner

// forgeGateway is the subset of *forge.Client the pass calls, named so
// tests can see at a glance what apply asks of the forge without importing
// the whole client.
type forgeGateway interface {
	Protection(ctx context.Context, branch string) (gates.Protection, error)
	InstallationToken(ctx context.Context) (string, time.Time, error)
	PullNumbersForCommit(ctx context.Context, sha string) ([]int, error)
	PullRequest(ctx context.Context, number int) (gates.PullRequest, error)
	Reviews(ctx context.Context, number int) ([]gates.Review, error)
	Commit(ctx context.Context, sha string) (gates.Commit, error)
}

var _ forgeGateway = (*forge.Client)(nil)

// applyDeps bundles everything the pass needs, real or faked. cmdApply
// builds the real set; tests build their own.
type applyDeps struct {
	Cfg         config.Config
	Dir         secrets.Dir
	Journal     *ledger.Journal
	Forge       forgeGateway
	Telegram    notify.Telegram
	Git         gitDriver
	NewTofu     tofuFactory
	Now         func() time.Time
	Stderr      io.Writer
	VaultConfig secrets.KVConfig
	// PATH and HOME are copied explicitly from the environment cmdApply was
	// given, so tofu's own child process (Runner.Env, which never inherits
	// implicitly -- §2 item 9) can still find the tofu binary and its
	// plugin cache.
	PATH, HOME string
}

func (d applyDeps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// cmdApply replaces apply.sh in full (§4.9, landing at step 5). Everything
// up to and including finding HEAD is a boot-time refusal with no
// heartbeat -- §2 item 5, "applied/HEAD is never guessed", is a refusal to
// START, the same class as an invalid config, and the reference bash
// itself never writes a heartbeat for either. Once HEAD is known, the pass
// always writes one (§2 item 8), which is runApplyPass's job and why this
// function stops doing its own error handling the moment that call is
// made.
func cmdApply(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: truss apply")
		return 2
	}

	cfg, problems := config.Load(getenv)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	dir := secrets.Dir{Root: cfg.SecretsDir}

	store, err := buildLedgerStore(cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	forgeClient, err := buildForgeClient(cfg, getenv)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	tg, err := loadTelegram(dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	vcfg, vproblems := loadVaultConfig(getenv)
	if len(vproblems) > 0 {
		for _, p := range vproblems {
			fmt.Fprintln(stderr, p)
		}
		return 1
	}

	journal := &ledger.Journal{Store: store, Layout: layoutFor(cfg)}
	last, err := journal.Head(ctx)
	if err != nil {
		if errors.Is(err, ledger.ErrNotFound) {
			fmt.Fprintf(stderr, "refusing to start: no %s in the ledger -- bootstrap/bootstrap.sh writes it, and it must never be guessed\n", cfg.LedgerHeadKey)
		} else {
			fmt.Fprintf(stderr, "refusing to start: could not read HEAD from the ledger: %v\n", err)
		}
		return 1
	}

	deps := applyDeps{
		Cfg:      cfg,
		Dir:      dir,
		Journal:  journal,
		Forge:    forgeClient,
		Telegram: tg,
		Git:      execGit{Bin: "git", Dir: cfg.Workdir, Stderr: stderr},
		NewTofu: func(env []string) tofuRunner {
			return plan.Runner{Bin: "tofu", PluginDir: cfg.PluginDir, Stderr: stderr, Env: env}
		},
		Now:         time.Now,
		Stderr:      stderr,
		VaultConfig: vcfg,
		PATH:        getenv("PATH"),
		HOME:        getenv("HOME"),
	}

	result := runApplyPass(ctx, deps, last)
	if result.notifyText != "" {
		fmt.Fprintln(stdout, result.notifyText)
	}
	if result.failure != "" {
		return 1
	}
	return 0
}

// applyResult is everything cmdApply needs to compute an exit code, after
// runApplyPass has already written the heartbeat and sent the alert.
type applyResult struct {
	failure    string
	notifyText string
}

// runApplyPass is the pass itself: the gate, the commit loop, rotation,
// drift, the expiry sweep, then ALWAYS a heartbeat and an alert (§2 item
// 8) -- there is no return path out of this function that skips them.
func runApplyPass(ctx context.Context, d applyDeps, last string) applyResult {
	var (
		appliedCount, noopCount int
		failure                 string
		rotationSummary         any = map[string]string{"skipped": "not a drift run"}
		driftSummary            any = map[string]string{"skipped": "not a drift run"}
		rotatedChanges          int
		drifted, errored        []string
		driftRun                = d.Cfg.DriftOnly
		driftSkipped            string
	)

	gateOK := false
	prot, err := d.Forge.Protection(ctx, "main")
	if err != nil {
		failure = fmt.Sprintf("could not read branch protection for main: %v", err)
	} else if problems := gates.CheckProtection(prot, d.Cfg.RequiredCheck); len(problems) > 0 {
		failure = "branch protection on main does not meet the bar: " + strings.Join(problems, "; ")
	} else {
		gateOK = true
	}

	if !gateOK {
		rotationSummary = map[string]string{"skipped": "branch protection gate failed"}
		driftSummary = map[string]string{"skipped": "branch protection gate failed"}
	} else if driftRun {
		rotationSummary = map[string]string{"skipped": "drift run"}
		drifted, errored, driftSkipped = runDrift(ctx, d, last)
		if driftSkipped != "" {
			driftSummary = map[string]string{"skipped": driftSkipped}
		} else {
			driftSummary = map[string]any{"drifted": orEmpty(drifted), "errored": orEmpty(errored)}
		}
	} else {
		// lockContended is deliberately not surfaced beyond stopping the
		// loop early: §2 item 7 says contention files no failed/<sha>,
		// sends no failure alert and leaves HEAD unmoved, which
		// runCommitLoop already guarantees by returning an empty failure
		// and the pre-contention HEAD.
		newLast, applied, noop, loopFailure, credentialsAppliedAt, _ := runCommitLoop(ctx, d, last)
		last = newLast
		appliedCount = applied
		noopCount = noop
		if loopFailure != "" {
			failure = loopFailure
		}

		driftSummary = map[string]string{"skipped": "not a drift run"}
		summary, changes, rotErr := runRotation(ctx, d, last, credentialsAppliedAt)
		rotationSummary = summary
		rotatedChanges = changes
		if rotErr != nil && failure == "" {
			failure = fmt.Sprintf("rotation of credentials at %s: %v", last, rotErr)
		}
	}

	// The expiry sweep runs on every pass, drift or not, gate-passed or
	// not -- check_credential_lifetimes is unconditional in the reference
	// (apply.sh:834-847), because a hand-held credential lapsing takes the
	// whole applier down regardless of what else happened this run.
	// §4.7, §2 item 16: the sweep never reports a clean bill it did not
	// earn. Its problem is REPORTED, never swallowed as "nothing is
	// expiring" -- but it does not set failure.
	//
	// ⚠️ IT USED TO SET failure, AND THAT WOULD HAVE MADE EVERY PRODUCTION
	// PASS RED. Nothing seeds `expires` into Vault yet, so the sweep's
	// "lists N items but not one records an expiry" fires on every run:
	// exit 1 and a Telegram FAILED every five minutes, ~288 a day. Both
	// 2026-09-08 reviewers called that a security cost rather than noise --
	// an alert channel nobody reads is where a real digest-gate refusal goes
	// to die -- and it also undid §2 item 7, because a contended pass
	// correctly leaves failure empty and this then filled it in. The
	// reference bash never set failure for it either.
	//
	// ⚠️ AND THE FINDINGS ARE TAKEN EVEN WHEN THE SWEEP ERRORED. Sweep.Run's
	// contract (and TestAPartialSweepReturnsBothItsFindingsAndItsError) is
	// that a caller gets BOTH; the Cloudflare probe runs first precisely so
	// the hand-made mint token's expiry survives an unreadable mount, and
	// the previous code then discarded it. That threw away the one
	// credential whose lapse takes the applier down, exactly when the vault
	// was misbehaving.
	var expiryUnavailable string
	expiring, sweepErr := runExpirySweep(ctx, d.Cfg, d.Dir, d.VaultConfig, d.now)
	if sweepErr != nil {
		expiryUnavailable = sweepErr.Error()
	}

	rotationJSON, _ := json.Marshal(rotationSummary)
	driftJSON, _ := json.Marshal(driftSummary)
	var failurePtr *string
	if failure != "" {
		failurePtr = &failure
	}
	hb := ledger.Heartbeat{
		Time:     d.now().UTC().Format("2006-01-02T15:04:05Z"),
		LastSHA:  last,
		Applied:  appliedCount,
		Noop:     noopCount,
		Failure:  failurePtr,
		Rotation: rotationJSON,
		Drift:    driftJSON,
		Expiring: toLedgerExpiring(expiring),
	}
	// The heartbeat write itself is best-effort in the sense that matters
	// here: a failure to WRITE it must not stop the alert from being sent,
	// because the alert is the one channel that still reaches somebody
	// when the ledger itself is the thing that broke.
	if err := d.Journal.PutHeartbeat(ctx, hb); err != nil && failure == "" {
		failure = fmt.Sprintf("writing the heartbeat: %v", err)
	}

	report := notify.Report{
		Subject:           "platform applier",
		LastSHA:           last,
		Applied:           appliedCount,
		Noop:              noopCount,
		Failure:           failure,
		DriftRun:          driftRun,
		DriftSkipped:      driftSkipped,
		Drifted:           drifted,
		Errored:           errored,
		RotatedChanges:    rotatedChanges,
		Expiring:          toNotifyExpiring(expiring),
		ExpiryUnavailable: expiryUnavailable,
	}
	text := notify.Compose(report)
	_ = d.Telegram.Send(ctx, text) // non-fatal, matching send_telegram (apply.sh:445-448)

	return applyResult{failure: failure, notifyText: text}
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// toLedgerExpiring converts the sweep's Name+DaysLeft findings into
// ledger.Expiring's Name+Expires shape. ledger.Expiring's own doc records
// that its shape is "not specified further by §4.2" and rides as "opaque
// data this package only needs to marshal in the right position" -- the
// two packages' Expiring types disagree (Name+Expires vs. Name+DaysLeft)
// because internal/ledger was specified before internal/secrets settled on
// the days-left shape. This is that disagreement's one crossing point.
func toLedgerExpiring(in []secrets.Expiring) []ledger.Expiring {
	out := make([]ledger.Expiring, len(in))
	for i, e := range in {
		expires := "no expiry recorded"
		if e.DaysLeft != nil {
			expires = "in " + strconv.Itoa(*e.DaysLeft) + "d"
		}
		out[i] = ledger.Expiring{Name: e.Name, Expires: expires}
	}
	return out
}

// runCommitLoop walks every commit from last (exclusive) to origin/main
// (inclusive), applying each one's touched roots in order. It returns the
// new HEAD, the applied/noop counts, a failure reason (if the pass must
// stop), the sha at which the credentials root was last applied THIS run
// (rotation's own "already applied this run" check), and whether it
// stopped because of state-lock contention -- which is not a failure (§2
// item 7): no failed/<sha> is filed, HEAD is not advanced past the
// contended commit, and the returned failure is empty.
func runCommitLoop(ctx context.Context, d applyDeps, last string) (newLast string, applied, noop int, failure string, credentialsAppliedAt string, lockContended bool) {
	tok, _, err := d.Forge.InstallationToken(ctx)
	if err != nil {
		return last, 0, 0, fmt.Sprintf("could not mint an installation token: %v", err), "", false
	}
	repoURL := "https://github.com/" + d.Cfg.Repo + ".git"
	if err := d.Git.EnsureClone(ctx, repoURL, tok); err != nil {
		return last, 0, 0, fmt.Sprintf("could not clone %s: %v", d.Cfg.Repo, err), "", false
	}
	if err := d.Git.Fetch(ctx, "origin", "main", tok); err != nil {
		return last, 0, 0, fmt.Sprintf("could not fetch origin main: %v", err), "", false
	}

	// §3 item 1: a failed rev-list is a refusal here, not the silent empty
	// queue the bash's `mapfile` produced -- the vacuous-pass shape
	// AGENTS.md already has a standing rule against.
	commits, err := d.Git.Commits(ctx, last, "origin/main")
	if err != nil {
		return last, 0, 0, fmt.Sprintf("could not list commits from %s to origin/main: %v", last, err), "", false
	}

	cc := &credCache{dir: d.Dir}
	baseEnv := buildBaseEnv(d)

	for _, sha := range commits {
		changedFiles, err := d.Git.ChangedFiles(ctx, sha)
		if err != nil {
			return last, applied, noop, fmt.Sprintf("could not read changed files for %s: %v", sha, err), credentialsAppliedAt, false
		}
		treeRoots, err := d.Git.TreeRoots(ctx, sha)
		if err != nil {
			return last, applied, noop, fmt.Sprintf("could not read the tree for %s: %v", sha, err), credentialsAppliedAt, false
		}
		roots := repo.TouchedRoots(changedFiles, treeRoots)

		if len(roots) == 0 {
			if err := d.Journal.PutNoop(ctx, sha); err != nil {
				return last, applied, noop, fmt.Sprintf("could not record noop for %s: %v", sha, err), credentialsAppliedAt, false
			}
			if err := d.Journal.AdvanceHead(ctx, sha); err != nil {
				return last, applied, noop, fmt.Sprintf("could not advance head to %s: %v", sha, err), credentialsAppliedAt, false
			}
			last = sha
			noop++
			continue
		}

		headSHA, _, reason, gateErr := checkCommitGate(ctx, d.Forge, d.Cfg.Approver, sha)
		if gateErr != nil {
			reason = gateErr.Error()
		}
		if reason != "" {
			if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
				reason = reason + fmt.Sprintf(" (and could not record the failure: %v)", err)
			}
			return last, applied, noop, reason, credentialsAppliedAt, false
		}

		if err := d.Git.Checkout(ctx, headSHA); err != nil {
			reason := fmt.Sprintf("could not check out %s: %v", headSHA, err)
			_ = d.Journal.PutFailed(ctx, sha, reason)
			return last, applied, noop, reason, credentialsAppliedAt, false
		}

		summaries := map[string]ledger.RootSummary{}
		for _, root := range roots {
			summary, lockBusy, reason := applyOneRoot(ctx, d, cc, baseEnv, headSHA, root)
			if lockBusy {
				return last, applied, noop, "", credentialsAppliedAt, true
			}
			if reason != "" {
				if err := d.Journal.PutFailed(ctx, sha, reason); err != nil {
					reason = reason + fmt.Sprintf(" (and could not record the failure: %v)", err)
				}
				return last, applied, noop, reason, credentialsAppliedAt, false
			}
			summaries[root] = summary
			if root == "credentials" {
				credentialsAppliedAt = sha
			}
		}

		if err := d.Journal.PutApplied(ctx, sha, summaries); err != nil {
			return last, applied, noop, fmt.Sprintf("could not record %s as applied: %v", sha, err), credentialsAppliedAt, false
		}
		if err := d.Journal.AdvanceHead(ctx, sha); err != nil {
			return last, applied, noop, fmt.Sprintf("could not advance head to %s: %v", sha, err), credentialsAppliedAt, false
		}
		last = sha
		applied++
	}

	return last, applied, noop, "", credentialsAppliedAt, false
}

// credCache lazily reads the "applying credentials" -- Google's key, the
// state-encryption passphrase and the Cloudflare mint token -- at most
// once per pass (§2 item 12: read only when the pass has work, which by
// the time credCache exists it does). cf-infra-admin is deliberately NOT
// cached here: apply_root re-reads it fresh before every non-credentials
// root, because credentials/ may have re-minted it earlier in this same
// pass (apply.sh's own comment on this, preserved in loadCFInfraAdminToken's
// caller).
type credCache struct {
	dir        secrets.Dir
	google     *string
	passphrase *string
	mint       *string
}

func (c *credCache) googleCreds() (string, error) {
	if c.google == nil {
		v, err := loadGCPCredentials(c.dir)
		if err != nil {
			return "", err
		}
		c.google = &v
	}
	return *c.google, nil
}

func (c *credCache) passphraseVal() (string, error) {
	if c.passphrase == nil {
		v, err := loadTofuPassphrase(c.dir)
		if err != nil {
			return "", err
		}
		c.passphrase = &v
	}
	return *c.passphrase, nil
}

func (c *credCache) mintToken() (string, error) {
	if c.mint == nil {
		v, err := loadCFMintToken(c.dir)
		if err != nil {
			return "", err
		}
		c.mint = &v
	}
	return *c.mint, nil
}

// buildBaseEnv is the part of every tofu invocation's environment that
// does not depend on the root: PATH and HOME, copied explicitly from the
// process environment because Runner.Env is never inherited implicitly
// (§2 item 9) -- so cmd/truss itself decides what crosses that boundary,
// rather than plan.Runner defaulting to os.Environ() by accident.
func buildBaseEnv(d applyDeps) []string {
	return []string{"PATH=" + d.PATH, "HOME=" + d.HOME}
}

// applyOneRoot runs init, plan, (for non-credentials roots) the digest
// gate, then apply for one root at one commit's head sha. It returns
// lockBusy=true, with no reason, exactly when tofu reported the state
// lock held elsewhere (§2 item 7) -- the caller must not file a failure
// for that case.
func applyOneRoot(ctx context.Context, d applyDeps, cc *credCache, baseEnv []string, headSHA, root string) (summary ledger.RootSummary, lockBusy bool, reason string) {
	if !d.Git.HasDir(root) {
		return ledger.RootSummary{}, false, fmt.Sprintf("root %s does not exist at %s", root, headSHA)
	}
	rootDir := filepath.Join(d.Cfg.Workdir, root)

	env := append([]string{}, baseEnv...)
	google, err := cc.googleCreds()
	if err != nil {
		return ledger.RootSummary{}, false, err.Error()
	}
	env = append(env, "GOOGLE_CREDENTIALS="+google)

	if root == "credentials" {
		mint, err := cc.mintToken()
		if err != nil {
			return ledger.RootSummary{}, false, err.Error()
		}
		pass, err := cc.passphraseVal()
		if err != nil {
			return ledger.RootSummary{}, false, err.Error()
		}
		env = append(env, "CLOUDFLARE_API_TOKEN="+mint, "TF_VAR_encryption_passphrase="+pass)
	} else {
		infra, ok, err := loadCFInfraAdminToken(cc.dir)
		if err != nil {
			return ledger.RootSummary{}, false, err.Error()
		}
		if !ok {
			return ledger.RootSummary{}, false, fmt.Sprintf(
				"cf-infra-admin is not in the platform vault: it is minted by credentials/, so that root must be applied before %s can be", root)
		}
		env = append(env, "CLOUDFLARE_API_TOKEN="+infra)
	}

	runner := d.NewTofu(env)
	const planFile = "tfplan"

	if err := runner.Init(ctx, rootDir); err != nil {
		if errors.Is(err, plan.ErrLockBusy) {
			return ledger.RootSummary{}, true, ""
		}
		return ledger.RootSummary{}, false, fmt.Sprintf("tofu init failed for %s: %v", root, err)
	}
	if err := runner.Plan(ctx, rootDir, planFile); err != nil {
		if errors.Is(err, plan.ErrLockBusy) {
			return ledger.RootSummary{}, true, ""
		}
		return ledger.RootSummary{}, false, fmt.Sprintf("tofu plan failed for %s: %v", root, err)
	}

	// §2 item 10: only credentials is exempt from the digest gate, because
	// CI never plans it.
	if root != "credentials" {
		planJSON, err := runner.ShowJSON(ctx, rootDir, planFile)
		if err != nil {
			if errors.Is(err, plan.ErrLockBusy) {
				return ledger.RootSummary{}, true, ""
			}
			return ledger.RootSummary{}, false, fmt.Sprintf("could not digest our own plan for %s: %v", root, err)
		}
		mine, err := plan.Digest(planJSON)
		if err != nil {
			return ledger.RootSummary{}, false, fmt.Sprintf("could not digest our own plan for %s: %v", root, err)
		}
		approved, err := d.Journal.ApprovedDigest(ctx, headSHA, root)
		approvedFound := true
		if err != nil {
			if errors.Is(err, ledger.ErrNotFound) {
				approvedFound = false
			} else {
				return ledger.RootSummary{}, false, fmt.Sprintf("could not read the approved plan digest for %s at %s: %v", root, headSHA, err)
			}
		}
		if problems := gates.CheckPlanDigest(root, headSHA, mine, approved, approvedFound); len(problems) > 0 {
			return ledger.RootSummary{}, false, strings.Join(problems, "; ")
		}
	}

	if err := runner.Apply(ctx, rootDir, planFile); err != nil {
		if errors.Is(err, plan.ErrLockBusy) {
			return ledger.RootSummary{}, true, ""
		}
		return ledger.RootSummary{}, false, fmt.Sprintf("tofu apply failed for %s: %v", root, err)
	}

	var n *int
	if pj, err := runner.ShowJSON(ctx, rootDir, planFile); err == nil {
		if count, ok := countResourceChanges(pj); ok {
			n = &count
		}
	}
	return ledger.RootSummary{ResourceChanges: n}, false, ""
}

// countResourceChanges mirrors summary_from_plan (apply.sh:598): the
// number of entries in the applied plan's own resource_changes array, or
// "could not be parsed" (nil, matching RootSummary.ResourceChanges' own
// doc) rather than zero.
func countResourceChanges(planJSON []byte) (int, bool) {
	var parsed struct {
		ResourceChanges []json.RawMessage `json:"resource_changes"`
	}
	if err := json.Unmarshal(planJSON, &parsed); err != nil {
		return 0, false
	}
	return len(parsed.ResourceChanges), true
}

// runRotation re-plans and, if a generation boundary passed, re-applies
// the credentials root at last -- never at origin/main (§2 item 14). It
// returns a JSON-marshalable summary (mirroring rotation_summary's shapes:
// {"skipped": "..."} or a RootSummary-shaped success, or {"failed": "..."}),
// the number of resource changes rotation made, and an error only when
// rotation itself failed (never for a skip, which is not a failure).
func runRotation(ctx context.Context, d applyDeps, last, credentialsAppliedAt string) (summary any, changes int, err error) {
	if !d.Git.HasDir("credentials") {
		return map[string]string{"skipped": "no credentials root at last applied commit"}, 0, nil
	}
	if credentialsAppliedAt == last && last != "" {
		return map[string]string{"skipped": "credentials applied this run at " + last}, 0, nil
	}

	if err := d.Git.Checkout(ctx, last); err != nil {
		return map[string]string{"failed": err.Error()}, 0, fmt.Errorf("could not check out %s for rotation: %w", last, err)
	}

	cc := &credCache{dir: d.Dir}
	baseEnv := buildBaseEnv(d)
	result, lockBusy, reason := applyOneRoot(ctx, d, cc, baseEnv, last, "credentials")
	if lockBusy {
		return map[string]string{"skipped": "state lock held elsewhere"}, 0, nil
	}
	if reason != "" {
		return map[string]string{"failed": reason}, 0, errors.New(reason)
	}
	if result.ResourceChanges != nil {
		changes = *result.ResourceChanges
	}
	return result, changes, nil
}

// runDrift plans every root at last WITHOUT applying (§2 item 15), under
// cf-infra-admin -- the same credential the roots apply under, so a plan
// failure here is drift or a real error, never a rejected credential. It
// returns the drifted and errored root lists, and -- when the check could
// not run at all -- a skipped reason, matching check_drift's own three
// outcomes (no roots, no cf-infra-admin yet, or a real sweep).
func runDrift(ctx context.Context, d applyDeps, last string) (drifted, errored []string, skipped string) {
	if err := d.Git.Checkout(ctx, last); err != nil {
		return nil, nil, fmt.Sprintf("could not check out %s: %v", last, err)
	}

	var roots []string
	if d.Git.HasDir("platform") {
		roots = append(roots, "platform")
	}
	treeRoots, err := d.Git.TreeRoots(ctx, last)
	if err != nil {
		return nil, nil, fmt.Sprintf("could not read the tree at %s: %v", last, err)
	}
	for _, r := range treeRoots {
		if strings.HasPrefix(r, "projects/") {
			roots = append(roots, r)
		}
	}
	if len(roots) == 0 {
		return nil, nil, "no roots at last applied commit"
	}

	infra, ok, err := loadCFInfraAdminToken(d.Dir)
	if err != nil {
		return nil, nil, fmt.Sprintf("could not read cf-infra-admin: %v", err)
	}
	if !ok {
		return nil, nil, "cf-infra-admin is not in the vault yet"
	}

	google, err := loadGCPCredentials(d.Dir)
	if err != nil {
		return nil, nil, fmt.Sprintf("could not read gcp-apply credentials: %v", err)
	}
	env := append(buildBaseEnv(d), "CLOUDFLARE_API_TOKEN="+infra, "GOOGLE_CREDENTIALS="+google)

	for _, root := range roots {
		rootDir := filepath.Join(d.Cfg.Workdir, root)
		runner := d.NewTofu(env)
		if err := runner.Init(ctx, rootDir); err != nil {
			errored = append(errored, root)
			continue
		}
		changed, err := runner.PlanDetailed(ctx, rootDir)
		if err != nil {
			errored = append(errored, root)
			continue
		}
		if changed {
			drifted = append(drifted, root)
		}
	}
	return drifted, errored, ""
}
