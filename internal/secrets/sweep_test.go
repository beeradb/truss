package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeStore is an in-memory Store, for testing Sweep.Run's own orchestration
// -- ordering, error propagation and the recorded-expiry gate -- without
// going through the wire.
type fakeStore struct {
	name    string
	items   []string
	listErr error

	expiry    map[string]fakeExpiry
	expiryErr map[string]error

	calls []string // items Expiry was actually called for
}

type fakeExpiry struct {
	raw      string
	recorded bool
}

func (f *fakeStore) Name() string { return f.name }

func (f *fakeStore) List(ctx context.Context) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.items, nil
}

func (f *fakeStore) Expiry(ctx context.Context, item string) (string, bool, error) {
	f.calls = append(f.calls, item)
	if err, ok := f.expiryErr[item]; ok {
		return "", false, err
	}
	e := f.expiry[item]
	return e.raw, e.recorded, nil
}

// fakeProbe is a Probe with a canned answer.
type fakeProbe struct {
	t   time.Time
	ok  bool
	err error
}

func (p fakeProbe) Expiry(ctx context.Context) (time.Time, bool, error) { return p.t, p.ok, p.err }

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

var testNow = time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

func daysFromTestNow(days int) string {
	return testNow.AddDate(0, 0, days).Format("2006-01-02")
}

func TestTheSweepFailureIsReturnedNotFatal(t *testing.T) {
	store := &fakeStore{name: "platform", listErr: errors.New("list denied")}
	sw := Sweep{Stores: []Store{store}, Now: fixedNow(testNow)}

	_, err := sw.Run(context.Background())
	if err == nil {
		t.Fatal("Run with a failing store = nil error, want an error returned to the caller")
	}
}

func TestTheSweepFailureNamesTheMountAndTheCallThatFailed(t *testing.T) {
	t.Run("a List failure names the mount", func(t *testing.T) {
		store := &fakeStore{name: "particular-mount", listErr: errors.New("boom")}
		sw := Sweep{Stores: []Store{store}, Now: fixedNow(testNow)}
		_, err := sw.Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "particular-mount") {
			t.Errorf("Run error = %v, want it to name %q", err, "particular-mount")
		}
	})

	t.Run("an Expiry failure names the mount and the item", func(t *testing.T) {
		store := &fakeStore{
			name:      "particular-mount",
			items:     []string{"particular-item"},
			expiryErr: map[string]error{"particular-item": errors.New("boom")},
		}
		sw := Sweep{Stores: []Store{store}, Now: fixedNow(testNow)}
		_, err := sw.Run(context.Background())
		if err == nil {
			t.Fatal("Run with a failing Expiry = nil error")
		}
		if !strings.Contains(err.Error(), "particular-mount") || !strings.Contains(err.Error(), "particular-item") {
			t.Errorf("Run error = %v, want it to name both the mount and the item", err)
		}
	})
}

func TestAPartialSweepReturnsBothItsFindingsAndItsError(t *testing.T) {
	good := &fakeStore{
		name:  "good-mount",
		items: []string{"soon-to-expire"},
		expiry: map[string]fakeExpiry{
			"soon-to-expire": {raw: daysFromTestNow(5), recorded: true},
		},
	}
	bad := &fakeStore{name: "bad-mount", listErr: errors.New("boom")}

	sw := Sweep{Stores: []Store{good, bad}, WarnDays: 30, Now: fixedNow(testNow)}
	findings, err := sw.Run(context.Background())

	if err == nil {
		t.Fatal("Run with one good store and one failing store = nil error")
	}
	if len(findings) != 1 || findings[0].Name != "soon-to-expire" {
		t.Fatalf("Run's findings = %+v, want the good store's finding preserved alongside the error", findings)
	}
}

func TestAMountWhereNothingRecordsAnExpiryIsAnErrorNotAListOfNulls(t *testing.T) {
	store := &fakeStore{
		name:  "unwired-mount",
		items: []string{"a", "b", "c"},
		expiry: map[string]fakeExpiry{
			"a": {raw: "", recorded: false},
			"b": {raw: "", recorded: false},
			"c": {raw: "", recorded: false},
		},
	}
	sw := Sweep{Stores: []Store{store}, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err == nil {
		t.Fatal("Run over a mount where nothing records an expiry = nil error")
	}
	if len(findings) != 0 {
		t.Errorf("Run returned %v alongside the refusal, want no findings from the unwired mount", findings)
	}
}

func TestOneRecordedExpiryDisarmsThatRefusal(t *testing.T) {
	store := &fakeStore{
		name:  "partially-wired-mount",
		items: []string{"a", "b"},
		expiry: map[string]fakeExpiry{
			"a": {raw: "", recorded: false},
			"b": {raw: "never", recorded: true},
		},
	}
	sw := Sweep{Stores: []Store{store}, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run with one recorded expiry in the mount = %v, want the refusal disarmed", err)
	}
	if len(findings) != 1 || findings[0].Name != "a" || findings[0].DaysLeft != nil {
		t.Errorf("Run findings = %+v, want exactly one null-days finding for the unrecorded item 'a'", findings)
	}
}

func TestProbedItemsAreSkippedInTheVaultScan(t *testing.T) {
	store := &fakeStore{
		name:  "platform",
		items: []string{"cf-token-mint", "some-other-item"},
		expiry: map[string]fakeExpiry{
			"cf-token-mint":   {raw: "2099-01-01", recorded: true}, // would be wrong to read
			"some-other-item": {raw: "never", recorded: true},
		},
	}
	sw := Sweep{
		Stores:   []Store{store},
		Probes:   map[string]Probe{"cf-token-mint": fakeProbe{ok: false}},
		WarnDays: 30,
		Now:      fixedNow(testNow),
	}

	if _, err := sw.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, called := range store.calls {
		if called == "cf-token-mint" {
			t.Errorf("Store.Expiry was called for cf-token-mint, a probed item -- its own issuer's answer must be the only one read")
		}
	}
}

func TestTheProbeStillReportsWhenTheMountIsUnreadable(t *testing.T) {
	within := testNow.AddDate(0, 0, 5)
	store := &fakeStore{name: "platform", listErr: errors.New("vault is down")}
	sw := Sweep{
		Stores:   []Store{store},
		Probes:   map[string]Probe{"cf-token-mint": fakeProbe{t: within, ok: true}},
		WarnDays: 30,
		Now:      fixedNow(testNow),
	}

	findings, err := sw.Run(context.Background())
	if err == nil {
		t.Fatal("Run with an unreadable mount = nil error")
	}
	if len(findings) != 1 || findings[0].Name != "cf-token-mint" {
		t.Fatalf("Run findings = %+v, want the probe's finding to have survived the mount failure", findings)
	}
}

func TestAProbeFailureIsNoExpiryRecordedNotAnError(t *testing.T) {
	sw := Sweep{
		Probes: map[string]Probe{"cf-token-mint": fakeProbe{err: errors.New("cloudflare unreachable")}},
		Now:    fixedNow(testNow),
	}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run with only a failing probe = %v, want no error -- a probe failure is a finding, not a fatal sweep error", err)
	}
	if len(findings) != 1 || findings[0].Name != "cf-token-mint" || findings[0].DaysLeft != nil {
		t.Fatalf("Run findings = %+v, want one null-days finding for cf-token-mint", findings)
	}
}

func TestNeverIsAnOptOut(t *testing.T) {
	store := &fakeStore{
		name:  "platform",
		items: []string{"a-bot-token"},
		expiry: map[string]fakeExpiry{
			"a-bot-token": {raw: "never", recorded: true},
		},
	}
	// A huge WarnDays would catch anything with a real date; "never" must
	// still never appear.
	sw := Sweep{Stores: []Store{store}, WarnDays: 1_000_000, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("Run findings = %+v, want 'never' excluded entirely", findings)
	}
}

func TestAnAbsentExpiryIsReportedWithNullDays(t *testing.T) {
	store := &fakeStore{
		name:  "platform",
		items: []string{"has-expiry", "no-expiry"},
		expiry: map[string]fakeExpiry{
			"has-expiry": {raw: daysFromTestNow(400), recorded: true}, // disarms the mount-wide refusal
			"no-expiry":  {raw: "", recorded: false},
		},
	}
	sw := Sweep{Stores: []Store{store}, WarnDays: 30, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, f := range findings {
		if f.Name == "no-expiry" {
			found = true
			if f.DaysLeft != nil {
				t.Errorf("finding for an item with no recorded expiry carried DaysLeft=%d, want nil", *f.DaysLeft)
			}
		}
	}
	if !found {
		t.Error("an item with no recorded expiry was skipped instead of reported")
	}
}

func TestAnUnparseableDateIsReportedNotSkipped(t *testing.T) {
	store := &fakeStore{
		name:  "platform",
		items: []string{"garbled"},
		expiry: map[string]fakeExpiry{
			"garbled": {raw: "not-a-real-date", recorded: true},
		},
	}
	sw := Sweep{Stores: []Store{store}, WarnDays: 30, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 1 || findings[0].Name != "garbled" || findings[0].DaysLeft != nil {
		t.Fatalf("Run findings = %+v, want one null-days finding for the unparseable date", findings)
	}
}

func TestABareDateAndAnRFC3339InstantBothParse(t *testing.T) {
	bare := "2026-12-01"
	instant := "2026-12-01T00:00:00Z"

	dBare, okBare := DaysUntil(testNow, bare)
	dInstant, okInstant := DaysUntil(testNow, instant)

	if !okBare {
		t.Errorf("DaysUntil(%q) did not parse", bare)
	}
	if !okInstant {
		t.Errorf("DaysUntil(%q) did not parse", instant)
	}
	if dBare != dInstant {
		t.Errorf("a bare date and midnight UTC as RFC3339 gave different day counts: %d vs %d", dBare, dInstant)
	}
}

func TestDaysUntilIsNegativeForThePast(t *testing.T) {
	raw := testNow.AddDate(0, 0, -10).Format(time.RFC3339)
	days, ok := DaysUntil(testNow, raw)
	if !ok {
		t.Fatalf("DaysUntil(%q) did not parse", raw)
	}
	if days >= 0 {
		t.Errorf("DaysUntil for a date 10 days in the past = %d, want negative", days)
	}
}

func TestDaysUntilTruncatesTowardZeroLikeTheBash(t *testing.T) {
	t.Run("12 hours past reads 0, not -1", func(t *testing.T) {
		raw := testNow.Add(-12 * time.Hour).Format(time.RFC3339)
		days, ok := DaysUntil(testNow, raw)
		if !ok {
			t.Fatalf("DaysUntil(%q) did not parse", raw)
		}
		if days != 0 {
			t.Errorf("DaysUntil 12h in the past = %d, want 0 (truncation toward zero, not floor)", days)
		}
	})

	t.Run("36 hours past reads -1, not -2", func(t *testing.T) {
		raw := testNow.Add(-36 * time.Hour).Format(time.RFC3339)
		days, ok := DaysUntil(testNow, raw)
		if !ok {
			t.Fatalf("DaysUntil(%q) did not parse", raw)
		}
		if days != -1 {
			t.Errorf("DaysUntil 36h in the past = %d, want -1", days)
		}
	})
}

func TestAnItemComfortablyInDateIsNotReported(t *testing.T) {
	store := &fakeStore{
		name:  "platform",
		items: []string{"fresh"},
		expiry: map[string]fakeExpiry{
			"fresh": {raw: daysFromTestNow(365), recorded: true},
		},
	}
	sw := Sweep{Stores: []Store{store}, WarnDays: 30, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("Run findings = %+v, want none -- 365 days out is comfortably in date at a 30-day warning", findings)
	}
}

func TestWarnDaysIsTheBoundaryInclusive(t *testing.T) {
	store := &fakeStore{
		name:  "platform",
		items: []string{"exactly-at-boundary", "one-past-boundary"},
		expiry: map[string]fakeExpiry{
			"exactly-at-boundary": {raw: daysFromTestNow(30), recorded: true},
			"one-past-boundary":   {raw: daysFromTestNow(31), recorded: true},
		},
	}
	sw := Sweep{Stores: []Store{store}, WarnDays: 30, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	names := map[string]bool{}
	for _, f := range findings {
		names[f.Name] = true
	}
	if !names["exactly-at-boundary"] {
		t.Error("an item with exactly WarnDays left was not reported -- the boundary must be inclusive")
	}
	if names["one-past-boundary"] {
		t.Error("an item with WarnDays+1 left was reported -- it is comfortably in date")
	}
}

func TestEachMountIsSweptInOrder(t *testing.T) {
	first := &fakeStore{
		name: "first-mount", items: []string{"first-item"},
		expiry: map[string]fakeExpiry{"first-item": {raw: daysFromTestNow(5), recorded: true}},
	}
	second := &fakeStore{
		name: "second-mount", items: []string{"second-item"},
		expiry: map[string]fakeExpiry{"second-item": {raw: daysFromTestNow(5), recorded: true}},
	}
	sw := Sweep{Stores: []Store{first, second}, WarnDays: 30, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(findings) != 2 || findings[0].Name != "first-item" || findings[1].Name != "second-item" {
		t.Fatalf("Run findings = %+v, want first-mount's finding before second-mount's", findings)
	}
}

func TestTheSameTitleInTwoMountsIsReportedTwice(t *testing.T) {
	first := &fakeStore{
		name: "platform", items: []string{"shared-item"},
		expiry: map[string]fakeExpiry{"shared-item": {raw: daysFromTestNow(5), recorded: true}},
	}
	second := &fakeStore{
		name: "recipes-runtime", items: []string{"shared-item"},
		expiry: map[string]fakeExpiry{"shared-item": {raw: daysFromTestNow(5), recorded: true}},
	}
	sw := Sweep{Stores: []Store{first, second}, WarnDays: 30, Now: fixedNow(testNow)}

	findings, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	count := 0
	for _, f := range findings {
		if f.Name == "shared-item" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("a title present in two mounts was reported %d time(s), want 2 -- the sweep must not dedupe", count)
	}
}

// TestAnEmptyMountIsNotACleanSweep: Vault answers a LIST over an empty
// prefix with 404 and KV.List reports that as zero items -- correctly,
// because that is how Vault says "empty". But zero items means the
// per-item loop never runs, so the no-expiry alarm cannot fire and the
// sweep returns "nothing is expiring" over a mount it never read.
//
// The applier reads its own credentials from this mount, so an empty
// listing means it was wiped, the path is wrong, or a policy denial is
// being read as emptiness. Raised by the 2026-09-08 security review as the
// clean-bill-not-earned case §4.7 did not cover.
func TestAnEmptyMountIsNotACleanSweep(t *testing.T) {
	sw := Sweep{
		Stores:   []Store{emptyStore{name: "platform"}},
		WarnDays: 30,
		Now:      fixedNow(testNow),
	}
	findings, err := sw.Run(context.Background())
	if err == nil {
		t.Fatal("an empty mount was reported as a clean sweep")
	}
	if !strings.Contains(err.Error(), "platform") {
		t.Errorf("the error does not name the mount: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("findings = %v, want none", findings)
	}
}

// emptyStore lists nothing, the way Vault's own 404-on-empty-prefix reaches
// Sweep through KV.List.
type emptyStore struct{ name string }

func (e emptyStore) Name() string                           { return e.name }
func (e emptyStore) List(context.Context) ([]string, error) { return nil, nil }
func (e emptyStore) Expiry(context.Context, string) (string, bool, error) {
	return "", false, errors.New("Expiry must not be called: there are no items")
}
