package metrics

import (
	"math"
	"strings"
	"testing"
)

func gauge(name, help string, v float64, labels ...Label) Family {
	return Family{Name: name, Help: help, Samples: []Sample{{Labels: labels, Value: v}}}
}

func TestAFamilyRendersItsHelpItsTypeAndItsSamples(t *testing.T) {
	got, err := Render(Set{{
		Name: "truss_pass_success",
		Help: "1 when the pass reported no failure.",
		Samples: []Sample{
			{Labels: []Label{{"root", "platform"}}, Value: 1},
			{Labels: []Label{{"root", "credentials"}}, Value: 0},
		},
	}})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	want := "# HELP truss_pass_success 1 when the pass reported no failure.\n" +
		"# TYPE truss_pass_success gauge\n" +
		"truss_pass_success{root=\"platform\"} 1\n" +
		"truss_pass_success{root=\"credentials\"} 0\n"
	if got != want {
		t.Errorf("Render() =\n%q\nwant\n%q", got, want)
	}
}

func TestASampleWithNoLabelsHasNoBraces(t *testing.T) {
	got, err := Render(Set{gauge("truss_pass_applied", "Commits applied.", 3)})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.HasSuffix(got, "truss_pass_applied 3\n") {
		t.Errorf("Render() = %q, want a bare `truss_pass_applied 3` line", got)
	}
}

// A Unix timestamp is the one value in this whole set that is big enough for
// Go's default float formatting to reach for an exponent, and Prometheus
// parses `1.7757792e+09` fine -- but a human reading a push body should not
// have to. More to the point, this test exists because the alternative
// formatting (%v on a float64) is what produces the exponent, and the
// staleness alert is built entirely on this number.
func TestATimestampRendersWithoutLosingPrecision(t *testing.T) {
	got, err := Render(Set{gauge("truss_pass_timestamp_seconds", "When the pass finished.", 1775779200)})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(got, "truss_pass_timestamp_seconds 1.7757792e+09\n") {
		t.Errorf("Render() = %q, want the exact timestamp", got)
	}
	// Whatever the spelling, it must parse back to the same second.
	if v := 1.7757792e+09; math.Abs(v-1775779200) > 0 {
		t.Fatalf("the spelling above is not the same number")
	}
}

func TestNaNAndTheInfinitiesAreSpelledTheWayTheFormatDefines(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{
		{math.NaN(), "NaN"},
		{math.Inf(1), "+Inf"},
		{math.Inf(-1), "-Inf"},
	} {
		got, err := Render(Set{gauge("truss_x", "help", tc.v)})
		if err != nil {
			t.Fatalf("Render(%v) error = %v", tc.v, err)
		}
		if !strings.Contains(got, "truss_x "+tc.want+"\n") {
			t.Errorf("Render(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
}

func TestAQuoteInALabelValueIsEscapedRatherThanEndingTheValue(t *testing.T) {
	got, err := Render(Set{gauge("truss_x", "help", 1, Label{"root", `pro"ject`})})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(got, `truss_x{root="pro\"ject"} 1`) {
		t.Errorf("Render() = %q, want the quote escaped", got)
	}
}

func TestANewlineInHelpIsEscapedRatherThanEndingTheComment(t *testing.T) {
	got, err := Render(Set{gauge("truss_x", "first\nsecond", 1)})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(got, `# HELP truss_x first\nsecond`) {
		t.Errorf("Render() = %q, want the newline escaped", got)
	}
	if strings.Count(got, "\n") != 3 {
		t.Errorf("Render() = %q, want exactly three lines", got)
	}
}

// The four refusals below are the whole reason Render returns an error. The
// Pushgateway answers a malformed body with one 400 for the entire request,
// so any of these reaching it would discard every other metric in the same
// push -- a telemetry channel failing open, silently, which is the exact
// shape this repository refuses.
func TestRenderRefusesTheThingsTheGatewayWouldThrowTheWholeBatchAwayFor(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  Set
		want string
	}{
		{
			name: "a metric name with a hyphen",
			set:  Set{gauge("truss-pass", "help", 1)},
			want: "not a valid metric name",
		},
		{
			name: "a metric name starting with a digit",
			set:  Set{gauge("1truss", "help", 1)},
			want: "not a valid metric name",
		},
		{
			name: "a label name with a colon",
			set:  Set{gauge("truss_x", "help", 1, Label{"a:b", "v"})},
			want: "not a valid label name",
		},
		{
			name: "a family with no help text",
			set:  Set{{Name: "truss_x", Samples: []Sample{{Value: 1}}}},
			want: "has no HELP text",
		},
		{
			name: "the same family declared twice",
			set:  Set{gauge("truss_x", "help", 1), gauge("truss_x", "help", 2)},
			want: "declared twice",
		},
		{
			name: "the same series pushed twice",
			set: Set{{Name: "truss_x", Help: "help", Samples: []Sample{
				{Labels: []Label{{"root", "platform"}}, Value: 1},
				{Labels: []Label{{"root", "platform"}}, Value: 2},
			}}},
			want: "pushed twice with the same labels",
		},
		{
			name: "the same series twice with its labels in the other order",
			set: Set{{Name: "truss_x", Help: "help", Samples: []Sample{
				{Labels: []Label{{"a", "1"}, {"b", "2"}}, Value: 1},
				{Labels: []Label{{"b", "2"}, {"a", "1"}}, Value: 2},
			}}},
			want: "pushed twice with the same labels",
		},
		{
			name: "a job label, which belongs to the grouping key",
			set:  Set{gauge("truss_x", "help", 1, Label{"job", "truss"})},
			want: "`job` label",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(tc.set)
			if err == nil {
				t.Fatalf("Render() error = nil, want one mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Render() error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// ⚠️ The Pushgateway rejects any push carrying a per-sample timestamp, and
// the exposition format allows one. Nothing in Render can emit it, and this
// is the test that says so out loud rather than leaving it to be re-derived
// from the absence of a field.
func TestNoSampleCarriesATimestamp(t *testing.T) {
	got, err := Render(Set{gauge("truss_pass_timestamp_seconds", "help", 1775779200)})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if n := len(strings.Fields(line)); n != 2 {
			t.Errorf("sample line %q has %d fields, want 2 -- a third is a timestamp", line, n)
		}
	}
}
