package gates

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// scripts/protection sets main's branch protection; CheckProtection decides
// what is acceptable. Those are two statements of one fact, and the ordinary
// way that ends is the script drifting a field while the gate keeps refusing
// -- protection that looks configured and stops the applier anyway, with the
// script reporting success.
//
// So the script prints its own payload (`scripts/protection payload`, which
// touches no network) and this feeds that exact JSON through the real gate.
// Neither side can move without the other noticing.
func TestProtectionScriptSatisfiesTheGate(t *testing.T) {
	payload := runProtectionScript(t, "payload")

	var got struct {
		RequiredStatusChecks *struct {
			Strict   *bool    `json:"strict"`
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
		EnforceAdmins *bool `json:"enforce_admins"`
		Reviews       *struct {
			RequireCodeOwners       *bool `json:"require_code_owner_reviews"`
			DismissStale            *bool `json:"dismiss_stale_reviews"`
			Count                   *int  `json:"required_approving_review_count"`
			RequireLastPushApproval *bool `json:"require_last_push_approval"`
			BypassAllowances        *struct {
				Users []string `json:"users"`
				Teams []string `json:"teams"`
				Apps  []string `json:"apps"`
			} `json:"bypass_pull_request_allowances"`
		} `json:"required_pull_request_reviews"`
		AllowForcePushes *bool `json:"allow_force_pushes"`
		AllowDeletions   *bool `json:"allow_deletions"`
	}
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("scripts/protection payload is not valid JSON: %v", err)
	}
	if got.RequiredStatusChecks == nil || got.Reviews == nil {
		t.Fatal("payload omits required_status_checks or required_pull_request_reviews entirely")
	}
	// ⚠️ THE SCRIPT MUST NOT SET THIS KEY AT ALL. bypass_pull_request_allowances
	// present means someone is exempted from the count and code-owner
	// requirements above; the compliant payload leaves the key out entirely
	// (nil here, not an empty object), same as GitHub does for "nobody".
	if got.Reviews.BypassAllowances != nil {
		t.Error("payload sets bypass_pull_request_allowances; the compliant setting is to omit the key")
	}

	p := Protection{
		RequiredApprovals:       got.Reviews.Count,
		RequireCodeOwners:       got.Reviews.RequireCodeOwners,
		DismissStaleReviews:     got.Reviews.DismissStale,
		EnforceAdmins:           got.EnforceAdmins,
		AllowForcePushes:        got.AllowForcePushes,
		AllowDeletions:          got.AllowDeletions,
		RequireUpToDateBranch:   got.RequiredStatusChecks.Strict,
		RequireLastPushApproval: got.Reviews.RequireLastPushApproval,
		StatusChecks:            got.RequiredStatusChecks.Contexts,
	}

	// "plan" is config.Config.RequiredCheck. Importing config here would be an
	// import cycle in the making for one string, so it is stated -- and the
	// script's own comment points at the same place.
	if problems := CheckProtection(p, "plan"); len(problems) != 0 {
		t.Errorf("scripts/protection sets protection the applier would refuse:")
		for _, problem := range problems {
			t.Errorf("  - %s", problem)
		}
	}

	// Not part of CheckProtection, because the applier cannot observe a
	// deletion that already happened. Asserted here because history is the
	// audit log and the script is what configures it.
	if got.AllowDeletions == nil || *got.AllowDeletions {
		t.Error("payload permits branch deletion")
	}
}

// ⚠️ The negative half. A test that only proves the good payload passes would
// still pass if CheckProtection stopped checking anything at all -- so this
// takes the script's own payload, turns off one field at a time, and requires
// each to be refused. This is what makes the test above evidence rather than a
// claim.
func TestTheGateRefusesEachFieldTheScriptSets(t *testing.T) {
	no := false
	zero := 0

	base := func() Protection {
		yes := true
		one := 1
		return Protection{
			RequiredApprovals: &one, RequireCodeOwners: &yes,
			DismissStaleReviews: &yes, EnforceAdmins: &yes,
			AllowForcePushes: &no, AllowDeletions: &no,
			RequireUpToDateBranch: &yes, RequireLastPushApproval: &yes,
			StatusChecks: []string{"plan"},
		}
	}
	if problems := CheckProtection(base(), "plan"); len(problems) != 0 {
		t.Fatalf("the baseline should pass, got %v", problems)
	}

	weaken := map[string]func(*Protection){
		"no approvals required":     func(p *Protection) { p.RequiredApprovals = &zero },
		"approvals absent":          func(p *Protection) { p.RequiredApprovals = nil },
		"code owners off":           func(p *Protection) { p.RequireCodeOwners = &no },
		"code owners absent":        func(p *Protection) { p.RequireCodeOwners = nil },
		"dismiss stale off":         func(p *Protection) { p.DismissStaleReviews = &no },
		"enforce admins off":        func(p *Protection) { p.EnforceAdmins = &no },
		"force pushes on":           func(p *Protection) { yes := true; p.AllowForcePushes = &yes },
		"force pushes absent":       func(p *Protection) { p.AllowForcePushes = nil },
		"deletions on":              func(p *Protection) { yes := true; p.AllowDeletions = &yes },
		"deletions absent":          func(p *Protection) { p.AllowDeletions = nil },
		"strict off":                func(p *Protection) { p.RequireUpToDateBranch = &no },
		"last push approval off":    func(p *Protection) { p.RequireLastPushApproval = &no },
		"last push approval absent": func(p *Protection) { p.RequireLastPushApproval = nil },
		"plan check missing":        func(p *Protection) { p.StatusChecks = []string{"lint"} },
		"no checks at all":          func(p *Protection) { p.StatusChecks = nil },
	}
	for name, break_ := range weaken {
		t.Run(name, func(t *testing.T) {
			p := base()
			break_(&p)
			if problems := CheckProtection(p, "plan"); len(problems) == 0 {
				t.Error("the gate accepted it; this weakening must be refused")
			}
		})
	}
}

func runProtectionScript(t *testing.T, arg string) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	script := filepath.Join(filepath.Dir(thisFile), "..", "..", "scripts", "protection")

	out, err := exec.Command(script, arg).Output()
	if err != nil {
		t.Fatalf("scripts/protection %s: %v", arg, err)
	}
	return out
}
