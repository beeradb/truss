package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/handoff"
	"github.com/beeradb/truss/internal/metrics"
	"github.com/beeradb/truss/internal/notify"
)

// gateway stands in for a Prometheus Pushgateway, recording every push. It
// deliberately does NOT parse the body: internal/metrics' own tests own the
// exposition format, and these tests own what truss chooses to say.
type gateway struct {
	mu     sync.Mutex
	pushes []gatewayPush
	srv    *httptest.Server
	status int
}

type gatewayPush struct {
	method, path, body string
}

func newGateway(t *testing.T) *gateway {
	t.Helper()
	g := &gateway{status: http.StatusOK}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.pushes = append(g.pushes, gatewayPush{r.Method, r.URL.Path, string(b)})
		status := g.status
		g.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *gateway) all() []gatewayPush {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]gatewayPush(nil), g.pushes...)
}

func (g *gateway) only(t *testing.T) gatewayPush {
	t.Helper()
	all := g.all()
	if len(all) != 1 {
		t.Fatalf("the gateway saw %d pushes, want exactly 1", len(all))
	}
	return all[0]
}

// sampleValue reads one series out of an exposition body. It matches the
// whole line rather than a prefix, so truss_pass_success cannot be answered
// by truss_pass_successfully_anything.
func sampleValue(t *testing.T, body, series string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, " ")
		if ok && name == series {
			return value
		}
	}
	t.Fatalf("no series %q in:\n%s", series, body)
	return ""
}

func hasSeries(body, series string) bool {
	for _, line := range strings.Split(body, "\n") {
		if name, _, ok := strings.Cut(line, " "); ok && name == series {
			return true
		}
	}
	return false
}

// --- what a pass says about itself ---------------------------------------

// TestAPassPushesUnderAGroupingKeyThatNamesWhichPassItWas is the one that
// keeps the daily pass's numbers alive. The two CronJobs describe different
// work -- one applies commits and never rotates, the other rotates and never
// applies -- and pushed under one grouping key the frequent pass would
// overwrite the daily one's rotation and expiry series five minutes after
// they were written.
func TestAPassPushesUnderAGroupingKeyThatNamesWhichPassItWas(t *testing.T) {
	t.Run("the frequent pass", func(t *testing.T) {
		g := newGateway(t)
		deps, _ := successPassDeps(t, "metricsha1")
		deps.Cfg.MetricsPushURL = g.srv.URL

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if result := runApplyPass(ctx, deps, "metricsha1"); result.failure != "" {
			t.Fatalf("result.failure = %q, want empty", result.failure)
		}

		push := g.only(t)
		if push.method != http.MethodPut {
			t.Errorf("method = %s, want PUT", push.method)
		}
		if want := "/metrics/job/truss/pass/frequent"; push.path != want {
			t.Errorf("path = %q, want %q", push.path, want)
		}
	})

	t.Run("the drift pass", func(t *testing.T) {
		g := newGateway(t)
		deps, _ := idlePassDeps(t)
		deps.Cfg.MetricsPushURL = g.srv.URL
		deps.Cfg.DriftOnly = true
		fh := &fakeHandoff{Resp: handoff.Response{Value: handoff.ValueSkipped}}
		deps.HandoffSocket = "fake-socket"
		deps.Handoff = fh.send

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		runApplyPass(ctx, deps, "metricsha2")

		push := g.only(t)
		if want := "/metrics/job/truss/pass/drift"; push.path != want {
			t.Errorf("path = %q, want %q", push.path, want)
		}
		if got := sampleValue(t, push.body, "truss_drift_ran"); got != "1" {
			t.Errorf("truss_drift_ran = %s, want 1", got)
		}
		if hasSeries(push.body, "truss_pass_drift_run") {
			t.Error("truss_pass_drift_run is back; the grouping key already says pass=drift")
		}
	})
}

// TestEveryPassPushesTheTimestampEveryAlertIsAnchoredTo. A Pushgateway
// serves the last thing it was given forever, so an applier that has stopped
// running entirely keeps reporting truss_pass_success 1. The only series
// that goes bad on its own when nothing pushes is the finish time.
func TestEveryPassPushesTheTimestampEveryAlertIsAnchoredTo(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps func(*testing.T) (applyDeps, *fakeTelegram)
	}{
		{"a successful pass", func(t *testing.T) (applyDeps, *fakeTelegram) { return successPassDeps(t, "tsshasuccess") }},
		{"an idle pass", idlePassDeps},
		{"a refused pass", failingPassDeps},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateway(t)
			deps, _ := tc.deps(t)
			deps.Cfg.MetricsPushURL = g.srv.URL

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			runApplyPass(ctx, deps, "tsshasuccess")

			body := g.only(t).body
			if v := sampleValue(t, body, "truss_pass_timestamp_seconds"); v == "" || v == "0" {
				t.Errorf("truss_pass_timestamp_seconds = %q, want the pass's finish time", v)
			}
		})
	}
}

// TestARefusedPassPushesTheClassOfItsRefusal is the reason the class label
// exists at all. The alert text is a sentence; a rule cannot filter on a
// sentence, and one that tried would be a regex over prose.
func TestARefusedPassPushesTheClassOfItsRefusal(t *testing.T) {
	g := newGateway(t)
	deps, _ := failingPassDeps(t)
	deps.Cfg.MetricsPushURL = g.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, "headsha1"); result.failure == "" {
		t.Fatal("result.failure is empty, want the branch-protection refusal")
	}

	body := g.only(t).body
	if got := sampleValue(t, body, `truss_pass_failure{class="protection"}`); got != "1" {
		t.Errorf("truss_pass_failure{class=\"protection\"} = %s, want 1\n%s", got, body)
	}
	if got := sampleValue(t, body, `truss_pass_failure{class="digest"}`); got != "0" {
		t.Errorf("truss_pass_failure{class=\"digest\"} = %s, want 0 -- nothing reached the digest gate", got)
	}
	if got := sampleValue(t, body, `truss_gate_ok{gate="protection"}`); got != "0" {
		t.Errorf("truss_gate_ok{gate=\"protection\"} = %s, want 0", got)
	}
	if got := sampleValue(t, body, "truss_pass_success"); got != "0" {
		t.Errorf("truss_pass_success = %s, want 0", got)
	}
}

// TestASuccessfulApplyReportsTheDigestItChecked. truss_digest_checks moving
// is how "the gate ran and agreed" is told apart from "the gate never ran",
// which is the distinction the whole project turns on.
func TestASuccessfulApplyReportsTheDigestItChecked(t *testing.T) {
	g := newGateway(t)
	deps, _ := successPassDeps(t, "digestcountsha")
	deps.Cfg.MetricsPushURL = g.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, "digestcountsha"); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}

	body := g.only(t).body
	if got := sampleValue(t, body, "truss_digest_checks"); got != "1" {
		t.Errorf("truss_digest_checks = %s, want 1\n%s", got, body)
	}
	if got := sampleValue(t, body, "truss_digest_refusals"); got != "0" {
		t.Errorf("truss_digest_refusals = %s, want 0", got)
	}
	if got := sampleValue(t, body, "truss_pass_commits_applied"); got != "1" {
		t.Errorf("truss_pass_commits_applied = %s, want 1", got)
	}
	if !hasSeries(body, `truss_root_duration_seconds{root="`+gateRoot+`",phase="apply"}`) {
		t.Errorf("no apply duration for %s in:\n%s", gateRoot, body)
	}
}

// TestNoGatewayConfiguredPushesNothing is the regression guard for every
// deployment that has not opted in: with METRICS_PUSH_URL unset, nothing
// about the pass changes and nothing is dialled.
func TestNoGatewayConfiguredPushesNothing(t *testing.T) {
	g := newGateway(t)
	deps, _ := successPassDeps(t, "nogatewaysha")
	deps.Cfg.MetricsPushURL = ""

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, "nogatewaysha"); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}
	if n := len(g.all()); n != 0 {
		t.Fatalf("the gateway saw %d pushes with no URL configured, want 0", n)
	}
}

// TestAGatewayThatRefusesThePushDoesNotFailThePass. Telemetry is the last
// thing a pass does and the least important thing it does: a monitoring
// endpoint being down must never be able to fail a pass that applied
// infrastructure correctly.
func TestAGatewayThatRefusesThePushDoesNotFailThePass(t *testing.T) {
	g := newGateway(t)
	g.status = http.StatusInternalServerError
	deps, _ := successPassDeps(t, "gatewaydownsha")
	deps.Cfg.MetricsPushURL = g.srv.URL
	stderr := &strings.Builder{}
	deps.Stderr = stderr

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := runApplyPass(ctx, deps, "gatewaydownsha")

	if result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- a broken gateway is not an apply failure", result.failure)
	}
	if !strings.Contains(stderr.String(), "metrics push failed") {
		t.Errorf("stderr = %q, want the failure narrated rather than swallowed", stderr.String())
	}
}

// --- the narration levels the log panels filter on ------------------------

// TestNarrationCarriesALevelAndAWarningIsCounted. "Show me every warning
// this week" has to be a query over a field, not a grep for whichever words
// the message happened to use -- and the count has to reach the metric set,
// or a pass that warned and then recovered leaves no trace at all.
func TestNarrationCarriesALevelAndAWarningIsCounted(t *testing.T) {
	g := newGateway(t)
	deps, ft := successPassDeps(t, "levelsha")
	deps.Cfg.MetricsPushURL = g.srv.URL
	stderr := &strings.Builder{}
	deps.Stderr = stderr
	// Closing the Telegram server makes the send fail, which is the
	// canonical non-fatal warning: the channel that reports every other
	// failure is the one whose own failure used to be silent.
	ft.srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, "levelsha"); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty -- a failed send is not an apply failure", result.failure)
	}

	out := stderr.String()
	if !strings.Contains(out, "level=info") {
		t.Errorf("stderr = %q, want ordinary narration at level=info", out)
	}
	if !strings.Contains(out, `level=warn msg="telegram send failed`) {
		t.Errorf("stderr = %q, want the failed send at level=warn", out)
	}

	body := g.only(t).body
	if got := sampleValue(t, body, `truss_pass_log_events{level="warn"}`); got == "0" {
		t.Errorf("truss_pass_log_events{level=\"warn\"} = 0, want the warning counted\n%s", body)
	}
}

// --- passMetrics itself, called directly ----------------------------------

func renderOrFail(t *testing.T, set metrics.Set) string {
	t.Helper()
	body, err := metrics.Render(set)
	if err != nil {
		t.Fatalf("the set this pass would push does not render: %v", err)
	}
	return body
}

func minimalMetrics(t *testing.T, rep notify.Report, o *passObs) string {
	t.Helper()
	return renderOrFail(t, passMetrics(time.Unix(1775779200, 0), 90*time.Second, true, rep, o, buildFacts{}))
}

// TestEveryFailureClassIsPushedEveryPassEvenTheOnesThatDidNotFire. A class
// that appeared only when it fired would leave a query returning "no data"
// for two different facts -- nothing went wrong, and nothing pushed at all.
func TestEveryFailureClassIsPushedEveryPassEvenTheOnesThatDidNotFire(t *testing.T) {
	body := minimalMetrics(t, notify.Report{}, newPassObs())
	for _, class := range failureClasses {
		series := `truss_pass_failure{class="` + class + `"}`
		if got := sampleValue(t, body, series); got != "0" {
			t.Errorf("%s = %s, want 0", series, got)
		}
	}
}

// TestOnePassCanReportMoreThanOneClass. Filing a rotation failure into the
// ledger can itself fail; both facts are real and neither replaces the other.
func TestOnePassCanReportMoreThanOneClass(t *testing.T) {
	o := newPassObs()
	o.failed(classRotation)
	o.failed(classLedger)

	body := minimalMetrics(t, notify.Report{Failure: "rotation failed"}, o)
	for _, class := range []string{classRotation, classLedger} {
		if got := sampleValue(t, body, `truss_pass_failure{class="`+class+`"}`); got != "1" {
			t.Errorf("class %s = %s, want 1", class, got)
		}
	}
	if got := sampleValue(t, body, `truss_pass_failure{class="apply"}`); got != "0" {
		t.Errorf("class apply = %s, want 0", got)
	}
}

// TestACredentialWithNoRecordedExpiryIsNotReportedAsZeroDaysLeft. secrets.
// Expiring carries DaysLeft as a *int precisely because "no expiry recorded"
// and "expires today" are different facts; collapsing them into one number
// would make the more urgent one indistinguishable from the vaguer one.
func TestACredentialWithNoRecordedExpiryIsNotReportedAsZeroDaysLeft(t *testing.T) {
	days := 3
	body := minimalMetrics(t, notify.Report{Expiring: []notify.Expiring{
		{Name: "cf-token-mint", DaysLeft: &days},
		{Name: "gh-app-key"},
	}}, newPassObs())

	if got := sampleValue(t, body, `truss_credential_days_left{credential="cf-token-mint"}`); got != "3" {
		t.Errorf("days left = %s, want 3", got)
	}
	if hasSeries(body, `truss_credential_days_left{credential="gh-app-key"}`) {
		t.Errorf("gh-app-key has a days_left series; it records no expiry at all\n%s", body)
	}
	if got := sampleValue(t, body, `truss_credential_expiry_unrecorded{credential="gh-app-key"}`); got != "1" {
		t.Errorf("unrecorded = %s, want 1", got)
	}
	if got := sampleValue(t, body, "truss_expiry_findings"); got != "2" {
		t.Errorf("truss_expiry_findings = %s, want 2", got)
	}
}

// TestACredentialThatHasAlreadyExpiredReportsANegativeNumber, rather than
// being clamped at zero -- "expired eleven days ago" and "expires today" want
// different responses.
//
// ⚠️ THE FIXTURE NAMES IN THIS FILE ARE SHORT ON PURPOSE. scripts/leakscan
// refuses `credential=<16+ characters>` as the shape of an assigned
// credential, and `truss_credential_days_left{credential="..."}` is exactly
// that shape. The guard is right -- a real credential name is precisely what
// must not reach this repository -- so the fixtures stay under the limit
// rather than the scanner gaining an exemption.
func TestACredentialThatHasAlreadyExpiredReportsANegativeNumber(t *testing.T) {
	days := -11
	body := minimalMetrics(t, notify.Report{Expiring: []notify.Expiring{
		{Name: "expired-root", DaysLeft: &days},
	}}, newPassObs())
	if got := sampleValue(t, body, `truss_credential_days_left{credential="expired-root"}`); got != "-11" {
		t.Errorf("days left = %s, want -11", got)
	}
}

// TestASweepThatCouldNotRunDoesNotReportItselfAsClean is §4.7's rule in
// metric form: the sweep never claims a clean bill it did not earn. A sweep
// that failed reports no findings, which looks exactly like a clean one, so
// the query has to read truss_expiry_sweep_ok first.
func TestASweepThatCouldNotRunDoesNotReportItselfAsClean(t *testing.T) {
	ok := minimalMetrics(t, notify.Report{}, newPassObs())
	if got := sampleValue(t, ok, "truss_expiry_sweep_ok"); got != "1" {
		t.Errorf("a completed sweep reports %s, want 1", got)
	}

	broken := minimalMetrics(t, notify.Report{ExpiryUnavailable: "vault would not answer"}, newPassObs())
	if got := sampleValue(t, broken, "truss_expiry_sweep_ok"); got != "0" {
		t.Errorf("an unavailable sweep reports %s, want 0", got)
	}
	if got := sampleValue(t, broken, "truss_expiry_findings"); got != "0" {
		t.Errorf("findings = %s, want 0 -- and that is exactly why sweep_ok has to be read first", got)
	}
}

// TestTheFrequentPassNeverClaimsACleanSweepItDidNotRun. The sweep is
// daily-only. A frequent pass that reported truss_expiry_sweep_ok 1 would be
// claiming a result for work it did not do -- and, pushed 288 times a day
// against the daily pass's one, would be the value most queries saw.
func TestTheFrequentPassNeverClaimsACleanSweepItDidNotRun(t *testing.T) {
	set := passMetrics(time.Unix(1775779200, 0), time.Second, false, notify.Report{}, newPassObs(), buildFacts{})
	body := renderOrFail(t, set)
	if got := sampleValue(t, body, "truss_expiry_sweep_ok"); got != "0" {
		t.Errorf("truss_expiry_sweep_ok = %s on a frequent pass, want 0", got)
	}
}

// TestADriftedRootAndARootThatCouldNotBePlannedAreDifferentSeries. "We
// looked and it drifted" and "we could not tell" are different claims; the
// heartbeat already keeps them apart and the metrics must not fold them.
func TestADriftedRootAndARootThatCouldNotBePlannedAreDifferentSeries(t *testing.T) {
	body := minimalMetrics(t, notify.Report{
		Drifted: []string{"platform"},
		Errored: []string{"projects/example"},
	}, newPassObs())

	if got := sampleValue(t, body, `truss_root_drifted{root="platform"}`); got != "1" {
		t.Errorf("drifted platform = %s, want 1", got)
	}
	if hasSeries(body, `truss_root_drifted{root="projects/example"}`) {
		t.Error("a root that could not be planned is reported as drifted")
	}
	if got := sampleValue(t, body, `truss_root_drift_errored{root="projects/example"}`); got != "1" {
		t.Errorf("errored projects/example = %s, want 1", got)
	}
}

// TestARootNamedTwiceDoesNotRenderTheSameSeriesTwice. The Pushgateway
// answers a duplicated series with one 400 for the WHOLE push, so a
// heartbeat that named a root twice would silently discard every metric in
// the same request.
func TestARootNamedTwiceDoesNotRenderTheSameSeriesTwice(t *testing.T) {
	set := passMetrics(time.Unix(1775779200, 0), time.Second, true, notify.Report{
		Drifted: []string{"platform", "platform"},
	}, newPassObs(), buildFacts{})
	if _, err := metrics.Render(set); err != nil {
		t.Fatalf("Render() error = %v, want the duplicate collapsed before it got here", err)
	}
}

// TestAPassWithNothingRecordedStillRenders. passMetrics is called with
// whatever the pass managed to observe, including nothing at all -- the
// gate-failed-at-the-first-call case. Rendering is the step that would
// otherwise fail closed on the exact pass whose metrics matter most.
func TestAPassWithNothingRecordedStillRenders(t *testing.T) {
	body := renderOrFail(t, passMetrics(time.Unix(1775779200, 0), 0, false, notify.Report{}, nil, buildFacts{}))
	if got := sampleValue(t, body, "truss_pass_success"); got != "1" {
		t.Errorf("truss_pass_success = %s, want 1", got)
	}
}

// TestAFailedPassAlwaysNamesAClass is the invariant behind the whole class
// label, and it is one no individual test above would have caught.
//
// ⚠️ A PASS THAT REPORTS truss_pass_success 0 WITH EVERY CLASS AT 0 IS THE
// FAIL-OPEN SHAPE IN MINIATURE. The dashboard shows red, the timeline shows
// nothing fired, and there is no query that says why -- "something went wrong
// and we cannot tell you what" reads on a panel exactly like a rendering
// quirk. Every site that sets a failure records its class at the point the
// cause is still known; this asserts that none was ever missed, and keeps
// asserting it when somebody adds the next one.
func TestAFailedPassAlwaysNamesAClass(t *testing.T) {
	// ⚠️ READ THE PUSHED BODY, NOT deps.Obs. runApplyPass takes applyDeps BY
	// VALUE and installs the recorder on its own copy, so the caller's
	// deps.Obs stays nil -- which is the type's contract ("nothing reads it
	// back") working as intended, and a trap for a test that tries to peek.
	// What the pass actually reported is what the gateway received.
	classesIn := func(t *testing.T, body string) []string {
		t.Helper()
		var named []string
		for _, c := range failureClasses {
			if sampleValue(t, body, `truss_pass_failure{class="`+c+`"}`) == "1" {
				named = append(named, c)
			}
		}
		return named
	}

	t.Run("a branch-protection refusal", func(t *testing.T) {
		g := newGateway(t)
		deps, _ := failingPassDeps(t)
		deps.Cfg.MetricsPushURL = g.srv.URL
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, "classsha1")

		if result.failure == "" {
			t.Fatal("this fixture is supposed to fail")
		}
		body := g.only(t).body
		if got := sampleValue(t, body, "truss_pass_success"); got != "0" {
			t.Fatalf("truss_pass_success = %s on a failed pass", got)
		}
		if named := classesIn(t, body); len(named) == 0 {
			t.Errorf("the pass failed with %q and named no class", result.failure)
		}
	})

	// ⚠️ THE ONE THAT MATTERS MOST: a plan that did not hash to the approved
	// one. If any refusal has to be attributable, it is this one.
	t.Run("a digest refusal", func(t *testing.T) {
		const sha = "classdigestsha"
		g := newGateway(t)
		deps, fl, _, _ := gateDeps(t, sha, sha)
		if ff, ok := deps.Forge.(*fakeForge); ok {
			ff.ProtectionResult = protectionCompliantForNow()
		}
		fl.put("digests/"+sha+"/"+gateSlug+".digest", []byte(notOurDigest(t)))
		deps.Cfg.MetricsPushURL = g.srv.URL

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		result := runApplyPass(ctx, deps, sha)

		if result.failure == "" {
			t.Fatal("a mismatched digest was applied")
		}
		body := g.only(t).body
		if got := sampleValue(t, body, "truss_digest_refusals"); got != "1" {
			t.Errorf("truss_digest_refusals = %s, want 1", got)
		}
		named := classesIn(t, body)
		if len(named) == 0 {
			t.Fatalf("the pass refused %q and named no class", result.failure)
		}
		found := false
		for _, c := range named {
			if c == classDigest {
				found = true
			}
		}
		if !found {
			t.Errorf("a digest refusal was classed as %v, not %q", named, classDigest)
		}
	})

	// The other direction: a pass that succeeded must not be naming one.
	t.Run("a pass that succeeded names nothing", func(t *testing.T) {
		g := newGateway(t)
		deps, _ := successPassDeps(t, "classoksha")
		deps.Cfg.MetricsPushURL = g.srv.URL
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if result := runApplyPass(ctx, deps, "classoksha"); result.failure != "" {
			t.Fatalf("result.failure = %q, want empty", result.failure)
		}
		if named := classesIn(t, g.only(t).body); len(named) != 0 {
			t.Errorf("a clean pass named %v", named)
		}
	})
}

// TestTheQueueDepthIsWhatThePassFoundNotWhatItLeft. A pass that applies some
// of the queue and then fails should report what was waiting when it looked --
// that is the number somebody asks for after an alert.
func TestTheQueueDepthIsWhatThePassFoundNotWhatItLeft(t *testing.T) {
	g := newGateway(t)
	deps, _ := successPassDeps(t, "queuesha")
	deps.Cfg.MetricsPushURL = g.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, "queuesha"); result.failure != "" {
		t.Fatalf("result.failure = %q, want empty", result.failure)
	}

	body := g.only(t).body
	if got := sampleValue(t, body, "truss_queue_depth"); got != "1" {
		t.Errorf("truss_queue_depth = %s, want 1 -- one commit was waiting", got)
	}
}

// TestAPassThatNeverReachedTheQueueReportsNoDepth. ⚠️ THE ABSENCE IS THE
// POINT. A pass refused at the branch-protection gate never ran the commit
// loop and knows nothing about how much work is waiting. Reporting 0 would be
// a claim it did not earn, and would read identically to a queue that is
// genuinely empty -- which is the difference between "nothing to do" and
// "we are not looking".
func TestAPassThatNeverReachedTheQueueReportsNoDepth(t *testing.T) {
	g := newGateway(t)
	deps, _ := failingPassDeps(t)
	deps.Cfg.MetricsPushURL = g.srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result := runApplyPass(ctx, deps, "queuerefusedsha"); result.failure == "" {
		t.Fatal("this fixture is supposed to be refused")
	}

	if hasSeries(g.only(t).body, "truss_queue_depth") {
		t.Error("a pass refused before the commit loop reported a queue depth it never measured")
	}
}
