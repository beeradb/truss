package rotate

import (
	"testing"
	"time"
)

// Every case below is expressed as an offset from the epoch, never a
// hard-coded date -- the arithmetic is what is under test, not a calendar.
var (
	epoch  = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	period = 45 * 24 * time.Hour
)

func clock() Clock {
	return Clock{Epoch: epoch, Period: period}
}

func TestCurrent(t *testing.T) {
	cases := []struct {
		name    string
		offset  time.Duration
		wantN   int
		wantErr bool
	}{
		{"at the epoch", 0, 0, false},
		{"one second before the first boundary", period - time.Second, 0, false},
		{"exactly at the first boundary", period, 1, false},
		{"deep in the sequence", 7*period + time.Hour, 7, false},
		{"before the epoch", -time.Second, 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := epoch.Add(tc.offset)
			g, err := clock().Current(at)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Current(%s) = %+v, want error", at, g)
				}
				return
			}
			if err != nil {
				t.Fatalf("Current(%s) returned error: %v", at, err)
			}
			if g.N != tc.wantN {
				t.Errorf("Current(%s).N = %d, want %d", at, g.N, tc.wantN)
			}
			if g.Name() != generationName(tc.wantN) {
				t.Errorf("Current(%s).Name() = %q, want %q", at, g.Name(), generationName(tc.wantN))
			}
		})
	}
}

func generationName(n int) string {
	return Generation{N: n}.Name()
}

func TestLive(t *testing.T) {
	cases := []struct {
		name   string
		offset time.Duration
		want   []int // generation numbers, current first
	}{
		{"at the epoch: no predecessor", 0, []int{0}},
		{"one second before the first boundary: still g0", period - time.Second, []int{0}},
		{"exactly at the first boundary", period, []int{1, 0}},
		{"deep in the sequence", 7*period + time.Hour, []int{7, 6}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := epoch.Add(tc.offset)
			live, err := clock().Live(at)
			if err != nil {
				t.Fatalf("Live(%s) returned error: %v", at, err)
			}
			if len(live) != len(tc.want) {
				t.Fatalf("Live(%s) = %d generations %v, want %d", at, len(live), live, len(tc.want))
			}
			for i, wantN := range tc.want {
				if live[i].N != wantN {
					t.Errorf("Live(%s)[%d].N = %d, want %d", at, i, live[i].N, wantN)
				}
			}
		})
	}
}

func TestLiveBeforeEpochIsAnError(t *testing.T) {
	if _, err := clock().Live(epoch.Add(-time.Second)); err == nil {
		t.Fatal("Live before epoch: want error, got none")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name      string
		c         Clock
		wantCount int
	}{
		{"valid clock", clock(), 0},
		{"zero epoch", Clock{Epoch: time.Time{}, Period: period}, 1},
		{"zero period", Clock{Epoch: epoch, Period: 0}, 1},
		{"negative period", Clock{Epoch: epoch, Period: -time.Hour}, 1},
		{"both wrong", Clock{}, 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := tc.c.Validate()
			if len(problems) != tc.wantCount {
				t.Errorf("Validate() = %v, want %d problem(s)", problems, tc.wantCount)
			}
		})
	}
}

func TestNextBoundary(t *testing.T) {
	cases := []struct {
		name   string
		offset time.Duration
		want   time.Duration // offset from epoch of the expected boundary
	}{
		{"at the epoch: the epoch itself is a boundary", 0, 0},
		{"mid-period: the next boundary", period / 2, period},
		{"exactly on a boundary: that instant, not the next one", period, period},
		{"mid second period", period + time.Hour, 2 * period},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := epoch.Add(tc.offset)
			got, err := clock().NextBoundary(at)
			if err != nil {
				t.Fatalf("NextBoundary(%s) returned error: %v", at, err)
			}
			want := epoch.Add(tc.want)
			if !got.Equal(want) {
				t.Errorf("NextBoundary(%s) = %s, want %s", at, got, want)
			}
		})
	}
}

func TestNextBoundaryBeforeEpochIsAnError(t *testing.T) {
	if _, err := clock().NextBoundary(epoch.Add(-time.Second)); err == nil {
		t.Fatal("NextBoundary before epoch: want error, got none")
	}
}

// TestTheSameInstantGivesTheSameAnswerInAnyZone is the property the
// offline-evaluation claim in the package doc rests on: two parties in two
// zones evaluating the same instant must agree, because the applier and CI
// must never disagree about which generation is live.
func TestTheSameInstantGivesTheSameAnswerInAnyZone(t *testing.T) {
	utcLoc := time.UTC
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	at := epoch.Add(7*period + time.Hour)
	inUTC := at.In(utcLoc)
	inTokyo := at.In(tokyo)

	c := clock()

	gotUTC, errUTC := c.Current(inUTC)
	gotTokyo, errTokyo := c.Current(inTokyo)
	if errUTC != nil || errTokyo != nil {
		t.Fatalf("Current returned errors: utc=%v tokyo=%v", errUTC, errTokyo)
	}
	if gotUTC.N != gotTokyo.N || !gotUTC.Begins.Equal(gotTokyo.Begins) || !gotUTC.Retires.Equal(gotTokyo.Retires) {
		t.Errorf("Current disagrees across zones: utc=%+v tokyo=%+v", gotUTC, gotTokyo)
	}

	liveUTC, err := c.Live(inUTC)
	if err != nil {
		t.Fatalf("Live(utc) returned error: %v", err)
	}
	liveTokyo, err := c.Live(inTokyo)
	if err != nil {
		t.Fatalf("Live(tokyo) returned error: %v", err)
	}
	if len(liveUTC) != len(liveTokyo) {
		t.Fatalf("Live disagrees across zones: utc=%v tokyo=%v", liveUTC, liveTokyo)
	}
	for i := range liveUTC {
		if liveUTC[i].N != liveTokyo[i].N {
			t.Errorf("Live[%d] disagrees across zones: utc=%+v tokyo=%+v", i, liveUTC[i], liveTokyo[i])
		}
	}

	nbUTC, err := c.NextBoundary(inUTC)
	if err != nil {
		t.Fatalf("NextBoundary(utc) returned error: %v", err)
	}
	nbTokyo, err := c.NextBoundary(inTokyo)
	if err != nil {
		t.Fatalf("NextBoundary(tokyo) returned error: %v", err)
	}
	if !nbUTC.Equal(nbTokyo) {
		t.Errorf("NextBoundary disagrees across zones: utc=%s tokyo=%s", nbUTC, nbTokyo)
	}
}

// TestAGenerationIsLiveForExactlyTwoPeriods pins the overlap window that is
// the entire point of the design: a generation must be live for two full
// periods, not one, so a replacement is always in place before its
// predecessor dies.
func TestAGenerationIsLiveForExactlyTwoPeriods(t *testing.T) {
	c := clock()
	g, err := c.Current(epoch.Add(7 * period))
	if err != nil {
		t.Fatalf("Current returned error: %v", err)
	}
	if g.N != 7 {
		t.Fatalf("Current(epoch+7*period).N = %d, want 7", g.N)
	}
	if got := g.Retires.Sub(g.Begins); got != 2*period {
		t.Errorf("Retires - Begins = %s, want %s", got, 2*period)
	}

	mustBeLive := func(at time.Time, label string) {
		t.Helper()
		live, err := c.Live(at)
		if err != nil {
			t.Fatalf("Live(%s) returned error: %v", label, err)
		}
		for _, l := range live {
			if l.N == g.N {
				return
			}
		}
		t.Errorf("g%d not in Live(%s) = %v", g.N, label, live)
	}
	mustNotBeLive := func(at time.Time, label string) {
		t.Helper()
		live, err := c.Live(at)
		if err != nil {
			t.Fatalf("Live(%s) returned error: %v", label, err)
		}
		for _, l := range live {
			if l.N == g.N {
				t.Errorf("g%d still in Live(%s) = %v, want gone", g.N, label, live)
			}
		}
	}

	mustBeLive(g.Begins, "at Begins")
	mustBeLive(g.Retires.Add(-time.Second), "one second before Retires")
	mustNotBeLive(g.Retires, "at Retires")
}
