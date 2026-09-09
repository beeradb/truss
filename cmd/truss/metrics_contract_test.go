package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/metrics"
	"github.com/beeradb/truss/internal/notify"
)

// ⚠️ THE DASHBOARDS AND THE RULES NAME METRICS THIS CODE EMITS, AND NOTHING
// ELSE MAKES THEM CHANGE TOGETHER. Rename a metric and every panel that used
// it renders "No data" -- a monitoring surface failing open while looking
// exactly like a quiet week. `internal/plan/digest.go` already carries this
// exact hazard against the consumer's jq, and the answer there was the same:
// a test that fails the moment the two disagree.
//
// This is that test in both directions. Every truss_ series an artifact
// mentions must be one passMetrics can emit, and every series passMetrics
// emits must be mentioned by at least one artifact -- an unwatched metric is
// a metric nobody will notice the absence of either.

// fullMetricSet is passMetrics driven with every optional family populated,
// which is the widest set the pass can produce. A family only emitted under
// some condition -- a drifted root, a credential with no recorded expiry --
// has to be in here or the contract below cannot see it.
func fullMetricSet(t *testing.T) metrics.Set {
	t.Helper()
	o := newPassObs()
	for _, c := range failureClasses {
		o.failed(c)
	}
	o.gate("protection", true)
	o.gate("rulesets", true)
	o.rootTook("platform", "apply", 1)
	o.rootChanged("platform", 2)
	o.rootFailed("platform")
	o.digestChecked(true)
	o.contended()
	o.drifting()
	o.rotated(true, true)
	o.publishAttempted()
	o.publishResult(true, 3)
	o.logged(levelWarn)
	o.ledgerError()

	days := 5
	return passMetrics(time.Unix(1775779200, 0), time.Minute, true, notify.Report{
		Applied: 1, Noop: 1, RotatedChanges: 2,
		Drifted: []string{"platform"},
		Errored: []string{"credentials"},
		Expiring: []notify.Expiring{
			{Name: "minted", DaysLeft: &days},
			{Name: "hand-made"},
		},
	}, o, buildFacts{GoVersion: "go1.25.0"})
}

// observabilityFiles is every artifact that names a metric: the Grafana
// dashboards and the Prometheus rules.
func observabilityFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	root := filepath.Join("..", "..", "observability")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Ext(path) {
		case ".json", ".yml", ".yaml":
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("no dashboards or rules found under %s", root)
	}
	return files
}

var trussMetricRef = regexp.MustCompile(`truss_[a-z0-9_]+`)

func emittedNames(t *testing.T) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, f := range fullMetricSet(t) {
		names[f.Name] = true
	}
	return names
}

func TestEveryMetricADashboardOrRuleNamesIsOneTrussEmits(t *testing.T) {
	emitted := emittedNames(t)
	for path, body := range observabilityFiles(t) {
		for _, ref := range trussMetricRef.FindAllString(body, -1) {
			if !emitted[ref] {
				t.Errorf("%s names %s, which passMetrics does not emit", filepath.Base(path), ref)
			}
		}
	}
}

func TestEveryMetricTrussEmitsIsWatchedBySomething(t *testing.T) {
	all := strings.Join(valuesOf(observabilityFiles(t)), "\n")
	var unwatched []string
	for name := range emittedNames(t) {
		// Matched as a whole word: truss_pass_commits_applied must not be
		// answered by a panel that happens to mention
		// truss_pass_commits_applied_somewhere_else.
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(all) {
			unwatched = append(unwatched, name)
		}
	}
	sort.Strings(unwatched)
	if len(unwatched) > 0 {
		t.Errorf("truss emits these and no dashboard or rule reads them: %s\n"+
			"Either put them on a panel or stop pushing them -- a metric nobody "+
			"watches is a metric nobody will notice the absence of.",
			strings.Join(unwatched, ", "))
	}
}

// TestEveryFailureClassIsWatched. The class label is the whole reason a
// dashboard can tell a digest refusal from a `tofu apply` that returned
// non-zero. A class no rule and no panel selects is a refusal that happens
// silently.
func TestEveryFailureClassIsWatched(t *testing.T) {
	all := strings.Join(valuesOf(observabilityFiles(t)), "\n")
	// A panel selecting the whole family (`truss_pass_failure{pass="..."}`)
	// covers every class at once, which is how the timeline dashboard does it.
	if strings.Contains(all, `truss_pass_failure{pass=`) {
		return
	}
	for _, class := range failureClasses {
		if !strings.Contains(all, class) {
			t.Errorf("no dashboard or rule mentions the %q failure class", class)
		}
	}
}

// TestEveryDashboardParsesAndStaysPortable. A dashboard is JSON somebody
// pastes into Grafana; a truncated one fails at import with a parser message
// and no clue which file. And a dashboard that names a datasource by uid
// imports into any other Grafana as a wall of "datasource not found", which
// is why each of these declares a datasource template variable instead.
//
// ⚠️ THE VARIABLE'S TYPE MUST MATCH WHAT THE PANELS ACTUALLY QUERY, WHICH IS
// STRICTER THAN "HAS A VARIABLE" AND IS THE VERSION THAT CATCHES SOMETHING.
// truss-logs.json queries Loki; had it reused the Prometheus dashboards'
// `datasource` variable, the picker would have offered a Prometheus and every
// panel would have failed at query time rather than at import -- the slowest
// possible way to find out.
func TestEveryDashboardParsesAndStaysPortable(t *testing.T) {
	for path, body := range observabilityFiles(t) {
		if filepath.Ext(path) != ".json" {
			continue
		}
		name := filepath.Base(path)

		var dash struct {
			UID        string `json:"uid"`
			Title      string `json:"title"`
			Templating struct {
				List []struct {
					Name  string `json:"name"`
					Type  string `json:"type"`
					Query string `json:"query"`
				} `json:"list"`
			} `json:"templating"`
			Panels []struct {
				Type    string `json:"type"`
				Targets []struct {
					Datasource struct {
						Type string `json:"type"`
					} `json:"datasource"`
					Expr string `json:"expr"`
				} `json:"targets"`
			} `json:"panels"`
		}
		if err := json.Unmarshal([]byte(body), &dash); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
			continue
		}
		if dash.UID == "" || dash.Title == "" {
			t.Errorf("%s has no uid or no title", name)
		}
		if strings.Contains(body, `"uid": "P`) {
			t.Errorf("%s looks like it carries a baked datasource uid", name)
		}

		// The datasource types this dashboard actually queries.
		want := map[string]bool{}
		for _, p := range dash.Panels {
			for _, tgt := range p.Targets {
				if tgt.Expr != "" && tgt.Datasource.Type != "" {
					want[tgt.Datasource.Type] = true
				}
			}
		}
		if len(want) == 0 {
			t.Errorf("%s has no panel queries at all", name)
		}

		offered := map[string]bool{}
		for _, v := range dash.Templating.List {
			if v.Type == "datasource" {
				offered[v.Query] = true
			}
		}
		for kind := range want {
			if !offered[kind] {
				t.Errorf("%s queries a %q datasource but its variables offer %v",
					name, kind, keysOf(offered))
			}
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func valuesOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
