// Package metrics renders the Prometheus text exposition format, and pushes
// it to a Pushgateway.
//
// ⚠️ TRUSS PUSHES BECAUSE NOTHING CAN SCRAPE IT. The pass is a CronJob that
// exists for the length of one run: by the time a scrape arrived, the process
// holding the numbers has exited. The Pushgateway is the component for that
// case and it accepts the same text a scrape would have returned, so this
// package renders that text and PUTs it.
//
// Rendering is a separate file from pushing for the reason internal/gates
// gives for itself: every interesting decision here -- what a sample is
// called, which labels it carries, whether the set is valid at all -- is then
// a function a test calls, with no server anywhere.
//
// ⚠️ EVERY SAMPLE IS A GAUGE, AND A COUNTER WOULD BE WRONG. A counter's
// meaning is "monotonically increasing since this process started"; this
// process starts, counts to three and exits, so the next pass would push a
// smaller number and Prometheus would read the drop as a counter reset. What
// truss can honestly report is the state of ONE pass, which is a gauge. The
// consequence for anybody writing a query is in observability/README.md:
// rate() and increase() do not mean what they usually mean here.
package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Label is one label on one sample.
type Label struct{ Name, Value string }

// Sample is one measurement: a value, and the labels that distinguish it
// from the other samples in its family.
type Sample struct {
	Labels []Label
	Value  float64
}

// Family is every sample sharing a metric name, with the HELP text a person
// reading the dashboard's metric browser will see.
//
// Help is required. An exposition without it is legal and it is also how a
// metric nobody can explain six months later gets born, which is the same
// failure this repository refuses everywhere else: a claim with no evidence
// attached. Render refuses a family with no Help.
type Family struct {
	Name    string
	Help    string
	Samples []Sample
}

// Set is the whole exposition one pass pushes.
type Set []Family

// Render writes set in the Prometheus text exposition format (version 0.0.4),
// or returns an error describing exactly what is malformed about it.
//
// ⚠️ IT REFUSES RATHER THAN EMITTING SOMETHING NEARLY RIGHT, because the
// Pushgateway answers a malformed body with one 400 for the WHOLE push. A
// single bad label name would therefore discard every other metric in the
// same request, and the only trace would be a status code in a log line
// nobody reads -- an entire telemetry channel failing open while reporting
// nothing. Every rule below is one the Pushgateway would otherwise enforce
// by throwing the batch away.
//
// ⚠️ NO TIMESTAMPS ARE EVER WRITTEN. The exposition format permits a
// per-sample millisecond timestamp and the Pushgateway rejects any push that
// carries one. When a pass wants to say when it ran, it says so in a sample's
// VALUE -- see truss_pass_timestamp_seconds -- which is also the only form an
// alerting rule can do arithmetic on.
func Render(set Set) (string, error) {
	var b strings.Builder
	seen := make(map[string]bool)
	names := make(map[string]bool)

	for _, f := range set {
		if !validMetricName(f.Name) {
			return "", fmt.Errorf("metrics: %q is not a valid metric name", f.Name)
		}
		if f.Help == "" {
			return "", fmt.Errorf("metrics: %s has no HELP text", f.Name)
		}
		if names[f.Name] {
			return "", fmt.Errorf("metrics: %s is declared twice; a family's samples must all be in one Family", f.Name)
		}
		names[f.Name] = true

		fmt.Fprintf(&b, "# HELP %s %s\n", f.Name, escapeHelp(f.Help))
		fmt.Fprintf(&b, "# TYPE %s gauge\n", f.Name)

		for _, s := range f.Samples {
			line, err := renderSample(f.Name, s)
			if err != nil {
				return "", err
			}
			// The identity a duplicate is judged on is name plus the
			// labels SORTED: {a="1",b="2"} and {b="2",a="1"} are one
			// series to Prometheus and would silently overwrite each
			// other, so they must collide here instead.
			key := f.Name + "\x00" + sortedLabelKey(s.Labels)
			if seen[key] {
				return "", fmt.Errorf("metrics: %s is pushed twice with the same labels", f.Name)
			}
			seen[key] = true
			b.WriteString(line)
		}
	}
	return b.String(), nil
}

func renderSample(name string, s Sample) (string, error) {
	var b strings.Builder
	b.WriteString(name)
	if len(s.Labels) > 0 {
		b.WriteByte('{')
		for i, l := range s.Labels {
			if !validLabelName(l.Name) {
				return "", fmt.Errorf("metrics: %s carries %q, which is not a valid label name", name, l.Name)
			}
			// ⚠️ `job` IS THE PUSHGATEWAY'S, NOT OURS. It takes job from
			// the grouping key in the URL path and answers a body that
			// also carries one with 400 for the whole push. Refused here,
			// where the message names the metric, rather than there, where
			// it is a status code.
			if l.Name == "job" {
				return "", fmt.Errorf("metrics: %s carries a `job` label; job comes from the push URL's grouping key", name)
			}
			if i > 0 {
				b.WriteByte(',')
			}
			// ⚠️ NOT %q, WHICH WOULD ESCAPE THE ESCAPES.
			// escapeLabelValue has already turned a quote into a
			// backslash-quote pair; handing that to %q escapes the
			// backslash a second time, so the value written no longer
			// matches the series anybody queries. The quotes go on by hand,
			// which is what TestAQuoteInALabelValueIsEscapedRatherThan-
			// EndingTheValue pins.
			b.WriteString(l.Name)
			b.WriteString(`="`)
			b.WriteString(escapeLabelValue(l.Value))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatValue(s.Value))
	b.WriteByte('\n')
	return b.String(), nil
}

// formatValue writes a float the way the exposition format defines it, which
// is Go's own syntax with three named exceptions. NaN and the infinities have
// no numeric spelling and must be written as those exact words.
func formatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "NaN"
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// escapeHelp escapes what HELP text may not contain: a newline would end the
// comment early and turn the rest into an unparseable line.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// escapeLabelValue escapes what a quoted label value may not contain. A root
// name or a refusal class reaches here from configuration, so this is not a
// theoretical case -- an unescaped quote would end the value early and make
// everything after it in the push unparseable.
func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func sortedLabelKey(labels []Label) string {
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, l.Name+"="+l.Value)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x00")
}

// validMetricName reports whether name matches [a-zA-Z_:][a-zA-Z0-9_:]*,
// which is what Prometheus accepts. Written as a loop rather than a regexp
// because the rule is two character classes and the loop is the shorter of
// the two to read.
func validMetricName(name string) bool { return validName(name, true) }

// validLabelName is the same rule without the colon: colons are reserved for
// names produced by recording rules, and a label may never contain one.
func validLabelName(name string) bool { return validName(name, false) }

func validName(name string, colonOK bool) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r == ':' && colonOK:
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
