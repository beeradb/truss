package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/beeradb/truss/internal/metrics"
)

// metricsContentType is the exposition format's own media type. Restated
// here rather than exported from internal/metrics/push.go, because that
// constant is about what the Pushgateway accepts and this is about what a
// scraper accepts -- the same string today, but two different reasons to
// write it, and internal/metrics stays free of anything HTTP-server-shaped.
const metricsContentType = "text/plain; version=0.0.4"

// snapshots holds the last metrics.Set each kind of pass produced, plus
// the loop-lifetime counts every /metrics response also carries. The
// loop's queue is serial -- exactly one pass runs at a time -- so there is
// exactly one writer; a scrape is concurrent with everything else, which
// is what makes the lock not optional.
type snapshots struct {
	mu       sync.RWMutex
	frequent metrics.Set // nil until the first frequent pass completes
	drift    metrics.Set // nil until the first drift pass completes
	counts   loopCounts

	started  time.Time
	inFlight atomic.Bool
}

func newSnapshots(started time.Time) *snapshots {
	return &snapshots{started: started}
}

// record is called with exactly the metrics.Set passMetrics produced for
// one pass -- the same value pushPassMetrics is handed, wired through
// applyDeps.Record (apply_cmd.go). One producer, two presentations.
func (s *snapshots) record(driftRun bool, set metrics.Set) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if driftRun {
		s.drift = set
		s.counts.drift++
	} else {
		s.frequent = set
		s.counts.frequent++
	}
}

// setInFlight is safe to call from any goroutine without the lock above --
// it is read by /metrics and by a future GET /status, never by the pass
// itself, so a plain atomic is enough and does not contend with record.
func (s *snapshots) setInFlight(v bool) { s.inFlight.Store(v) }

// render merges both snapshots into one exposition: every sample from
// each is labelled with which pass produced it, byte-for-byte the series
// shape the Pushgateway already produces from its own grouping key
// (pushPassMetrics, metrics.go) -- so every existing dashboard selector
// and alert expression keeps matching across the push-to-scrape
// transition. See metrics.go's package doc for why the push survives
// alongside this.
func (s *snapshots) render() (string, error) {
	s.mu.RLock()
	frequent, drift, counts := s.frequent, s.drift, s.counts
	started := s.started
	s.mu.RUnlock()

	// ⚠️ MERGED BY NAME, NOT CONCATENATED. Both snapshots emit families
	// under the SAME names (truss_pass_success, and so on); metrics.Render
	// refuses a name declared twice (render.go), so every sample sharing a
	// name -- one pass's, or both -- has to land in one Family, never two
	// Family values with the same Name.
	merged := newMergedFamilies()
	if frequent != nil {
		labelled, err := labelPass(frequent, "frequent")
		if err != nil {
			return "", err
		}
		merged.add(labelled)
	}
	if drift != nil {
		labelled, err := labelPass(drift, "drift")
		if err != nil {
			return "", err
		}
		merged.add(labelled)
	}
	set := merged.set()
	set = append(set, loopMetrics(started, counts, s.inFlight.Load())...)
	return metrics.Render(set)
}

// mergedFamilies combines Family values that share a Name into one,
// preserving the order names were first seen so the exposition is
// deterministic. Help and Kind are taken from whichever family declares
// the name first; passMetrics only ever describes one name one way, so
// the two snapshots never actually disagree about them.
type mergedFamilies struct {
	order  []string
	byName map[string]*metrics.Family
}

func newMergedFamilies() *mergedFamilies {
	return &mergedFamilies{byName: map[string]*metrics.Family{}}
}

func (m *mergedFamilies) add(set metrics.Set) {
	for _, f := range set {
		existing, ok := m.byName[f.Name]
		if !ok {
			copied := f
			copied.Samples = append([]metrics.Sample(nil), f.Samples...)
			m.byName[f.Name] = &copied
			m.order = append(m.order, f.Name)
			continue
		}
		existing.Samples = append(existing.Samples, f.Samples...)
	}
}

func (m *mergedFamilies) set() metrics.Set {
	out := make(metrics.Set, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, *m.byName[name])
	}
	return out
}

// labelPass copies pass's samples with pass="<name>" added to each one.
//
// ⚠️ IT COPIES THE LABEL SLICE. append(sample.Labels, passLabel) would, at
// any capacity slack, write into the backing array the snapshot itself
// still owns -- silent, intermittent, and it would show up as a
// pass="drift" label bleeding onto a frequent sample the next time both
// happened to share a moment in memory. make + copy is the only form that
// cannot alias.
func labelPass(set metrics.Set, pass string) (metrics.Set, error) {
	out := make(metrics.Set, len(set))
	for i, f := range set {
		samples := make([]metrics.Sample, len(f.Samples))
		for j, s := range f.Samples {
			for _, l := range s.Labels {
				if l.Name == "pass" {
					return nil, fmt.Errorf("metrics: %s already carries a pass label; pass is added by the snapshot, not by passMetrics", f.Name)
				}
			}
			labels := make([]metrics.Label, 0, len(s.Labels)+1)
			labels = append(labels, s.Labels...)
			labels = append(labels, metrics.Label{Name: "pass", Value: pass})
			samples[j] = metrics.Sample{Labels: labels, Value: s.Value}
		}
		out[i] = metrics.Family{Name: f.Name, Help: f.Help, Kind: f.Kind, Samples: samples}
	}
	return out, nil
}

const (
	metricsReadHeaderTimeout = 5 * time.Second
	metricsWriteTimeout      = 10 * time.Second
	metricsIdleTimeout       = 60 * time.Second
	metricsMaxHeaderBytes    = 16 << 10
)

// newMetricsServer builds the /metrics listener. It is never
// http.DefaultServeMux: any transitive import of net/http/pprof registers
// on that mux from init(), which would expose a heap dump -- containing
// every credential a pass just read -- on the pod network. An explicit
// mux is the only way to be sure that import cannot reach this listener
// even by accident.
func newMetricsServer(snap *snapshots) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		body, err := snap.render()
		if err != nil {
			// Render refuses rather than emitting something nearly right
			// (internal/metrics/render.go's own doc); a scrape gets a loud
			// 500, never a partial body.
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", metricsContentType)
		io.WriteString(w, body)
	})

	return &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: metricsReadHeaderTimeout,
		WriteTimeout:      metricsWriteTimeout,
		IdleTimeout:       metricsIdleTimeout,
		MaxHeaderBytes:    metricsMaxHeaderBytes,
		// The default ErrorLog writes to stderr in whatever shape net/http
		// chooses, which observability/dashboards' logfmt panels cannot
		// parse. Discarded rather than routed through applyDeps.log: this
		// server outlives any one pass, so it has no d to log through.
		ErrorLog: log.New(io.Discard, "", 0),
	}
}

// listenMetrics binds addr and starts serving, returning once the bind has
// either succeeded or failed -- a bind failure must be a synchronous
// refusal to start, never an error delivered to a goroutine nobody reads
// (the same reasoning internal/handoff/handoff.go's Serve gives for using
// net.ListenConfig instead of Server.ListenAndServe).
func listenMetrics(ctx context.Context, srv *http.Server, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: could not bind %s: %w", addr, err)
	}
	go srv.Serve(ln)
	return nil
}
