// Package parity drives `truss apply` over the same scenario corpus the
// reference bash applier is driven over, and asserts that the two produce
// the same final bucket and the same alert text (docs/port-plan.md §5.5).
//
// # How it works
//
// The reference repository's Python harness (tests/test_infra_pipeline.py)
// runs apply.sh for real, with `git`, `gh`, `tofu`, `aws`, `op` and `curl`
// replaced by PATH shims that answer out of a scenario's fixtures.json.
// capture/capture_plugin.py wraps that harness and records, per scenario,
// the inputs apply.sh was handed and the two outputs §5.5 names: the final
// bucket, key by key, and the Telegram text. Those recordings are the
// files in testdata/scenarios.
//
// This package replays each recording against the real truss binary. The
// binary is built and executed as a subprocess -- not called in-process --
// because the pass makes real subprocess calls to `tofu` and `git`, and
// reaches its ledger, its forge and its Vault over real HTTP. Faking those
// at the process boundary is what makes this a whole-pass test rather than
// a second copy of cmd/truss's unit tests: the argv plan.Runner builds, the
// exit codes execGit reads, the SigV4 the ledger signs with and the JSON
// the forge decoder pointer-preserves are all under test here, and none of
// them is under test when a Go fake is injected in their place.
//
// # Why the fakes are this package's own
//
// cmd/truss/testsupport_test.go already has a fake forge, ledger, Vault,
// git and tofu. They are not reused, and not because they live in package
// main: they are IN-PROCESS fakes (fakeForge implements the gateway
// interface directly; fakeGit and fakeTofu are structs the pass is handed),
// and this harness needs process-level ones -- HTTP servers and PATH
// shims. Moving them would have meant rewriting them anyway, into a shape
// their own callers do not want. The second reason is the repo's own rule
// that a fixture more forgiving than production invents failures and one
// stricter hides them: these fakes are driven by the reference
// fixtures.json schema, which is a different contract from "whatever this
// unit test needs", and sharing them would let a convenience added for a
// unit test quietly widen what parity accepts.
//
// # What is compared, and what is not
//
// Bucket contents and alert text, plus the exit code. Two exceptions are
// documented in §5.5 itself -- root ordering inside applied/<sha> (§3.4)
// and timestamps -- and everything else that differs must appear in
// divergences.go with a reason, or the test fails. See that file: it is
// the honest statement of how the two implementations differ, and it is as
// much the deliverable as the code is.
package parity

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Scenario is one recorded run of the reference applier: everything it was
// driven with, and everything it produced.
type Scenario struct {
	// Name identifies the scenario and is the Go subtest's name.
	Name string `json:"scenario"`
	// BashTest is the Python test function the recording came from. A
	// parametrised test contributes several scenarios sharing one BashTest.
	BashTest string `json:"bash_test"`

	Fixtures Fixtures `json:"fixtures"`

	// Env is the subset of the applier's environment the scenario sets,
	// already neutralised. UnsetEnv names variables the scenario removes,
	// which is how the refuse-to-start cases are expressed.
	Env      map[string]string `json:"env"`
	UnsetEnv []string          `json:"unset_env"`

	// WorkdirRoots are the root directories present on disk in the clone,
	// which is what `[ -d "$WORKDIR/$root" ]` and execGit.HasDir read.
	WorkdirRoots []string `json:"workdir_roots"`

	// BucketBefore is the ledger at the instant apply.sh started: the
	// scenario's own seeded objects plus the approved plan digests the
	// reference harness files with the real plan-digest tool.
	//
	// ⚠️ Seeding truss's fake bucket with the BASH's digest bytes is
	// deliberate and is a test in itself. truss computes its own digest
	// with internal/plan and compares; if the two implementations' bytes
	// ever diverge, every applying scenario fails here with a
	// digest-gate refusal, which is the loudest possible way for §5.2's
	// promise to come undone.
	BucketBefore map[string]string `json:"bucket_before"`

	Bash Outcome `json:"bash"`
}

// Fixtures is the reference fixtures.json schema, with the two fields that
// could not be carried verbatim converted. Everything else keeps its
// original name and shape so one corpus drives both implementations
// (§5.1).
type Fixtures struct {
	Commits          []string                   `json:"commits"`
	FilesChanged     map[string][]string        `json:"files_changed"`
	Tree             map[string][]string        `json:"tree"`
	PRs              map[string][]PR            `json:"prs"`
	Reviews          map[string][]Review        `json:"reviews"`
	CommitVerify     map[string]json.RawMessage `json:"commit_verification"`
	BranchProtection json.RawMessage            `json:"branch_protection"`
	TofuShow         json.RawMessage            `json:"tofu_show"`
	TofuPlanFail     bool                       `json:"tofu_plan_fail"`
	TofuApplyFail    bool                       `json:"tofu_apply_fail"`

	// Secrets replaces the reference fixture's `op_values`, which keys
	// its entries by a 1Password store URI. leakscan refuses a URI into a
	// concrete secret store, and cannot tell a fake one from a real one,
	// so the corpus carries vault -> item -> field -> value instead. A
	// nil value means the field is absent, which is how the reference
	// fixture spells "the item does not exist".
	Secrets map[string]map[string]map[string]*string `json:"secrets"`

	// CloudflareVerify is what Cloudflare's token-verify endpoint
	// answered for the hand-made minting token, without the URL it was
	// asked at (leakscan refuses a URL path into a real host). nil means
	// the scenario did not stub it, which the reference `curl` shim
	// answers with `{"ok":true}` -- a 200 carrying no expiry.
	CloudflareVerify *CloudflareVerify `json:"cloudflare_verify"`
}

// CloudflareVerify is the issuer's own answer about the minting token.
type CloudflareVerify struct {
	// ExpiresOn is a relative offset (see ResolveDate), or empty for "the
	// endpoint answered but named no expiry".
	ExpiresOn string `json:"expires_on"`
	// Unreadable makes the endpoint answer something that will not parse.
	Unreadable bool `json:"unreadable"`
}

// PR is one pull request as the forge reports it.
type PR struct {
	Number         int    `json:"number"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

// Review is one review on a pull request.
type Review struct {
	State string `json:"state"`
	User  struct {
		Login string `json:"login"`
	} `json:"user"`
	CommitID string `json:"commit_id"`
}

// Outcome is what one implementation's pass produced: the two surfaces
// §5.5 compares, plus the exit code.
type Outcome struct {
	ExitCode int `json:"exit_code"`
	// BucketAfter maps a ledger key to base64 of the object's raw bytes.
	//
	// ⚠️ BASE64, NOT THE TEXT. §5.1's own advice, for a concrete reason:
	// a plan digest is 64 lowercase hex characters and scripts/leakscan
	// refuses any 32+ character hex string. A testdata exemption would
	// blind the scanner over the whole directory, which is the guard with
	// a hole in it decision 9 refuses to build.
	BucketAfter map[string]string `json:"bucket_after"`
	// AlertB64 is the Telegram text, base64 of its raw bytes, or nil when
	// the pass refused before it could send one.
	//
	// ⚠️ ENCODED FOR THE SAME REASON BucketAfter IS, AND IT WAS NOT
	// OBVIOUS: the digest gate's refusal PRINTS BOTH DIGESTS INTO THE
	// ALERT ("approved <64 hex>, ours <64 hex>"), so one scenario's alert
	// is refused by leakscan's 32+ character hex rule exactly as a bucket
	// body would be. Found by running leakscan's own patterns over this
	// corpus before committing it.
	AlertB64 *string `json:"alert_b64"`
}

// AlertText decodes the recorded alert. ok is false when the pass sent
// none, which is a different fact from an empty one.
func (o Outcome) AlertText() (string, bool) {
	if o.AlertB64 == nil {
		return "", false
	}
	raw, err := Body(*o.AlertB64)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// WithAlert returns o carrying text as its alert.
func (o Outcome) WithAlert(text string) Outcome {
	encoded := Encode([]byte(text))
	o.AlertB64 = &encoded
	return o
}

// Body decodes one bucket object.
func Body(encoded string) ([]byte, error) { return base64.StdEncoding.DecodeString(encoded) }

// Encode is Body's inverse, used when recording what truss produced.
func Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// relativeDate matches the offset form the capture script writes in place
// of an absolute date: "@+300d", or "@-2d!" for a bare date with no time.
//
// ⚠️ AN ABSOLUTE DATE IN THE CORPUS WOULD ROT SILENTLY. Every expiry
// fixture in the reference harness is written as "now plus n days"; frozen
// into the corpus as an instant, a scenario that recorded "300 days left"
// would read as long expired a year from now, and the scenario would
// change what it tests without anybody editing it. Both replayers
// materialise these from their own clock instead.
var relativeDate = regexp.MustCompile(`^@([+-]\d+)d(!?)$`)

// ResolveDate turns a recorded offset into an absolute date at now. A
// value that is not an offset is returned unchanged, so a fixture that
// deliberately holds "never" or an unparseable string still says exactly
// what it said.
func ResolveDate(now time.Time, value string) string {
	m := relativeDate.FindStringSubmatch(value)
	if m == nil {
		return value
	}
	days, err := strconv.Atoi(m[1])
	if err != nil {
		return value
	}
	t := now.UTC().AddDate(0, 0, days)
	if m[2] == "!" {
		return t.Format("2006-01-02")
	}
	return t.Format("2006-01-02T15:04:05Z")
}

// Load reads every scenario in dir, sorted by name so subtests run in a
// stable order.
func Load(dir string) ([]Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Scenario
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var s Scenario
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		// Unknown fields are refused: a corpus recorded by a newer capture
		// script carrying a field this package ignores would silently test
		// less than it appears to.
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return nil, fmt.Errorf("parity: %s: %w", e.Name(), err)
		}
		if s.Name == "" {
			return nil, fmt.Errorf("parity: %s: no scenario name", e.Name())
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
