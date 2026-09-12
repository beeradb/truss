package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/beeradb/truss/internal/metrics"
)

func sampleSet(name string, v float64) metrics.Set {
	return metrics.Set{{Name: name, Help: "h", Samples: []metrics.Sample{{Value: v}}}}
}

// TestTheSnapshotServesNothingForAPassThatHasNotRunYet is
// truss_queue_depth's own reasoning (metrics.go), applied to the snapshot:
// a zero value for a pass kind that never ran would be a claim the loop
// did not earn, indistinguishable from a real zero.
func TestTheSnapshotServesNothingForAPassThatHasNotRunYet(t *testing.T) {
	snap := newSnapshots(time.Unix(0, 0))
	snap.record(false, sampleSet("truss_pass_success", 1))

	body, err := snap.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, `truss_pass_success{pass="frequent"} 1`) {
		t.Errorf("body = %q, want the frequent sample", body)
	}
	// truss_passes_total{pass="drift"} 0 is a DIFFERENT claim -- a
	// loop-lifetime counter honestly reporting "zero drift passes run" --
	// and legitimately appears (loopMetrics, metrics.go). What must not
	// appear is a PER-PASS family claiming a drift pass's own result.
	if strings.Contains(body, `truss_pass_success{pass="drift"}`) {
		t.Errorf("body = %q, want no truss_pass_success{pass=\"drift\"} before a drift pass has run", body)
	}
}

// TestEveryServedSampleCarriesItsPassLabel is the whole trick render()
// depends on: without it, two families with the same name and no
// distinguishing label would collide inside metrics.Render.
func TestEveryServedSampleCarriesItsPassLabel(t *testing.T) {
	snap := newSnapshots(time.Unix(0, 0))
	snap.record(false, sampleSet("truss_pass_success", 1))
	snap.record(true, sampleSet("truss_pass_success", 0))

	body, err := snap.render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, `truss_pass_success{pass="frequent"} 1`) {
		t.Errorf("missing frequent series in %q", body)
	}
	if !strings.Contains(body, `truss_pass_success{pass="drift"} 0`) {
		t.Errorf("missing drift series in %q", body)
	}
}

// TestLabellingOneSnapshotDoesNotMutateTheOther is the aliasing bug
// directly: labelPass must copy the label slice, not append into the
// stored sample's backing array.
func TestLabellingOneSnapshotDoesNotMutateTheOther(t *testing.T) {
	shared := metrics.Set{{Name: "truss_pass_success", Help: "h", Samples: []metrics.Sample{{Labels: make([]metrics.Label, 0, 4), Value: 1}}}}
	snap := newSnapshots(time.Unix(0, 0))
	snap.record(false, shared)
	snap.record(true, shared)

	if _, err := snap.render(); err != nil {
		t.Fatalf("render: %v", err)
	}

	snap.mu.RLock()
	frequentLabels := snap.frequent[0].Samples[0].Labels
	driftLabels := snap.drift[0].Samples[0].Labels
	snap.mu.RUnlock()

	if len(frequentLabels) != 0 {
		t.Errorf("stored frequent sample gained labels after render/record: %v -- labelPass must not alias the stored slice", frequentLabels)
	}
	if len(driftLabels) != 0 {
		t.Errorf("stored drift sample gained labels after render/record: %v", driftLabels)
	}
}

// TestASetThatAlreadyCarriesAPassLabelIsRefusedByName mirrors
// internal/metrics/render.go's refusal of a `job` label: pass is added by
// the snapshot, never by passMetrics.
func TestASetThatAlreadyCarriesAPassLabelIsRefusedByName(t *testing.T) {
	set := metrics.Set{{Name: "truss_x", Help: "h", Samples: []metrics.Sample{
		{Labels: []metrics.Label{{Name: "pass", Value: "frequent"}}, Value: 1},
	}}}
	if _, err := labelPass(set, "frequent"); err == nil {
		t.Fatalf("labelPass accepted a sample that already carries a pass label, want a refusal")
	}
}

// TestBothPassesRenderAsOneFamilyPerName proves render() merges same-named
// families from the two snapshots into one Family with two samples, rather
// than two Family values with the same name -- which metrics.Render
// refuses outright (render.go:84-86), the correct loud failure for a
// scrape rather than a silent one.
func TestBothPassesRenderAsOneFamilyPerName(t *testing.T) {
	snap := newSnapshots(time.Unix(0, 0))
	snap.record(false, sampleSet("truss_pass_success", 1))
	snap.record(true, sampleSet("truss_pass_success", 0))

	if _, err := snap.render(); err != nil {
		t.Fatalf("render: %v, want the merge to succeed rather than declaring truss_pass_success twice", err)
	}
}

// TestTheServedBodyIsThePushedBodyPlusThePassLabel is the transition
// guard: the day the scrape and the push disagree about what a pass
// produced, this goes red.
func TestTheServedBodyIsThePushedBodyPlusThePassLabel(t *testing.T) {
	set := sampleSet("truss_pass_success", 1)
	pushed, err := metrics.Render(set)
	if err != nil {
		t.Fatalf("Render (push shape): %v", err)
	}

	snap := newSnapshots(time.Unix(0, 0))
	snap.record(false, set)
	served, err := snap.render()
	if err != nil {
		t.Fatalf("render (scrape shape): %v", err)
	}

	wantLine := `truss_pass_success{pass="frequent"} 1`
	pushedLine := `truss_pass_success 1`
	if !strings.Contains(pushed, pushedLine) {
		t.Fatalf("pushed body = %q, want it to contain %q", pushed, pushedLine)
	}
	if !strings.Contains(served, wantLine) {
		t.Fatalf("served body = %q, want it to contain %q (the pushed line plus the pass label)", served, wantLine)
	}
}

// TestAScrapeIsSafeWhileAPassIsRecording is the concurrency guarantee: the
// queue is serial so there is one writer, but a scrape runs on its own
// goroutine and must never race it. Run with -race.
func TestAScrapeIsSafeWhileAPassIsRecording(t *testing.T) {
	snap := newSnapshots(time.Unix(0, 0))
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			snap.record(i%2 == 0, sampleSet("truss_pass_success", float64(i%2)))
		}
	}()
	for i := 0; i < 200; i++ {
		if _, err := snap.render(); err != nil {
			t.Fatalf("render: %v", err)
		}
	}
	<-done
}

// TestTheMetricsServerServesTheRenderedBody drives the real handler
// end-to-end through an httptest server, confirming the content type and
// that GET /metrics answers with exactly what render() produces.
func TestTheMetricsServerServesTheRenderedBody(t *testing.T) {
	snap := newSnapshots(time.Unix(0, 0))
	snap.record(false, sampleSet("truss_pass_success", 1))

	srv := newMetricsServer(snap)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.Header.Get("Content-Type") != metricsContentType {
		t.Errorf("Content-Type = %q, want %q", resp.Header.Get("Content-Type"), metricsContentType)
	}
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	if !strings.Contains(string(body[:n]), `truss_pass_success{pass="frequent"} 1`) {
		t.Errorf("body = %q, want the recorded sample", body[:n])
	}
}

// TestTheMetricsServerRefusesAnythingButGet mirrors Go 1.25's own
// method-aware mux behaviour, pinned so a future refactor away from
// "GET /metrics" cannot silently widen the route.
func TestTheMetricsServerRefusesAnythingButGet(t *testing.T) {
	snap := newSnapshots(time.Unix(0, 0))
	srv := newMetricsServer(snap)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// TestTheMetricsServerNeverExposesPprof guards newMetricsServer's use of
// an explicit http.NewServeMux rather than http.DefaultServeMux: any
// transitive import of net/http/pprof registers on the default mux from
// init(), which would put a heap dump -- containing every credential a
// pass just read -- on the pod network. This package does not import
// net/http/pprof today; the test exists so the day something does, this
// goes red instead of silently exposing it.
func TestTheMetricsServerNeverExposesPprof(t *testing.T) {
	snap := newSnapshots(time.Unix(0, 0))
	srv := newMetricsServer(snap)
	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/debug/pprof/heap")
	if err != nil {
		t.Fatalf("GET /debug/pprof/heap: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d: pprof must not be reachable through this listener", resp.StatusCode, http.StatusNotFound)
	}
}
