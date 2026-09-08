package parity

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Diff is one place the two implementations disagree, named precisely
// enough that an allowlist entry can be written about it and nothing
// broader.
type Diff struct {
	// Kind is "exit", "alert", "missing-key", "extra-key" or "value".
	Kind string
	// Key is the ledger key for a bucket diff, empty otherwise.
	Key string
	// Path is the JSON path inside that key, e.g. "/rotation/failed".
	// Empty means the object was compared as raw bytes.
	Path string
	// Bash and Truss are the two values, as strings.
	Bash, Truss string
}

func (d Diff) String() string {
	where := d.Kind
	if d.Key != "" {
		where += " " + d.Key
	}
	if d.Path != "" {
		where += " " + d.Path
	}
	return fmt.Sprintf("%s\n      bash:  %s\n      truss: %s", where, quote(d.Bash), quote(d.Truss))
}

func quote(s string) string {
	if len(s) > 400 {
		s = s[:400] + "…(truncated)"
	}
	return strconv.Quote(s)
}

// rotationKey is the ledger key a rotation failure is filed under. It
// embeds a UTC timestamp, so the KEY is a timestamp and falls under §5.5's
// second documented exception exactly as the values inside it do. Both
// recorders normalise it to the same placeholder.
var rotationKey = regexp.MustCompile(`^(.*/)rotation-\d{8}T\d{6}Z$`)

// NormaliseKey applies the timestamp exception to a ledger key.
func NormaliseKey(key string) string {
	if m := rotationKey.FindStringSubmatch(key); m != nil {
		return m[1] + "rotation-<ts>"
	}
	return key
}

// timestampPaths are the JSON fields §5.5's second documented exception
// covers: a value that is a wall-clock reading and can never be equal
// between two runs, let alone two implementations.
var timestampPaths = map[string]bool{
	"/time": true, // heartbeat: `date -u +%Y-%m-%dT%H:%M:%SZ`
	"/at":   true, // failed/<sha>: the same
}

// dayCountPath matches a heartbeat's expiring[i].days_left, which is a
// function of the clock rather than a fact about the pass. The two
// implementations read the clock milliseconds apart and both truncate
// toward zero, so a fixture written as "60 days from now" legitimately
// reads 59 in one and 60 in the other. It is compared with a tolerance of
// one day rather than dropped: a wrong day count is a real defect and
// dropping the field would hide it.
var dayCountPath = regexp.MustCompile(`^/expiring/\d+/days_left$`)

const dayCountTolerance = 1

// CompareOutcomes diffs what the bash produced against what truss
// produced, with the two documented exceptions applied and nothing else.
// Everything it returns is a disagreement that must be either a bug or an
// enumerated, argued divergence.
func CompareOutcomes(bash, truss Outcome) []Diff {
	var diffs []Diff

	if bash.ExitCode != truss.ExitCode {
		diffs = append(diffs, Diff{
			Kind:  "exit",
			Bash:  strconv.Itoa(bash.ExitCode),
			Truss: strconv.Itoa(truss.ExitCode),
		})
	}

	diffs = append(diffs, compareAlert(bash, truss)...)
	diffs = append(diffs, compareBuckets(bash.BucketAfter, truss.BucketAfter)...)
	return diffs
}

// expiryNotCheckedClause is §5.5's FOURTH documented exception, stated here
// rather than hidden in a divergences.go entry.
//
// truss inserts "; EXPIRY NOT CHECKED: <reason>" whenever the sweep cannot
// earn a clean bill, which today is EVERY pass -- nothing seeds `expires`
// into Vault yet (§4.7, "BLOCKED"). The bash has no such clause at all: it
// swept 1Password, where the field was hand-maintained.
//
// ⚠️ IT IS NORMALISED OUT RATHER THAN FORGIVEN BY AN ENTRY BECAUSE IT
// COMPOSES WITH EVERY OTHER ALERT DIVERGENCE. An entry matches one whole
// diff, so an alert differing BOTH by this clause and by (say) the approval
// wording is accepted by neither matcher, and the harness fails for a reason
// nobody can act on. Stripping it first lets the remaining entries see the
// difference they were written for.
//
// ⚠️ Nothing here checks the clause's CONTENT, so it is asserted positively
// elsewhere: cmd/truss's TestAnUnusableExpirySweepIsReportedAndDoesNotFail-
// ThePass requires it to appear and to name the reason, and
// TestACleanPassStaysCleanWithAnUnusableSweep requires it not to turn the
// pass red. A normalisation with no counterpart assertion would be this
// harness forgetting a whole clause exists.
var expiryNotCheckedRE = regexp.MustCompile(`; EXPIRY NOT CHECKED: secrets: [^;]*(?:; refusing[^;]*)?`)

func stripExpiryNotChecked(alert string) string {
	return expiryNotCheckedRE.ReplaceAllString(alert, "")
}

func compareAlert(bash, truss Outcome) []Diff {
	b, _ := bash.AlertText()
	t, _ := truss.AlertText()
	t = stripExpiryNotChecked(t)
	if b == t {
		return nil
	}
	return []Diff{{Kind: "alert", Bash: b, Truss: t}}
}

func compareBuckets(bash, truss map[string]string) []Diff {
	var diffs []Diff

	normalise := func(in map[string]string) map[string]string {
		out := make(map[string]string, len(in))
		for k, v := range in {
			out[NormaliseKey(k)] = v
		}
		return out
	}
	b, t := normalise(bash), normalise(truss)

	keys := map[string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range t {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	for _, key := range sorted {
		bv, inBash := b[key]
		tv, inTruss := t[key]
		switch {
		case inBash && !inTruss:
			diffs = append(diffs, Diff{Kind: "missing-key", Key: key, Bash: decode(bv)})
		case !inBash && inTruss:
			diffs = append(diffs, Diff{Kind: "extra-key", Key: key, Truss: decode(tv)})
		default:
			diffs = append(diffs, compareObject(key, decode(bv), decode(tv))...)
		}
	}
	return diffs
}

func decode(encoded string) string {
	raw, err := Body(encoded)
	if err != nil {
		return "<undecodable: " + encoded + ">"
	}
	return string(raw)
}

// compareObject compares one ledger object.
//
// ⚠️ A JSON OBJECT IS COMPARED FIELD BY FIELD, NOT BYTE BY BYTE, AND THAT
// IS A DELIBERATE WEAKENING WITH A NAMED REASON. jq pretty-prints and Go's
// encoding/json does not, so EVERY object the bash writes through jq
// differs from truss's in whitespace alone -- a byte comparison would fail
// on all of them, say nothing about their contents, and be the thing that
// gets quietly relaxed later. §3.4 already establishes that the applied
// record's serialisation may differ from the bash's, on the grounds that
// nothing hashes it; the same is true of the heartbeat and the failure
// record. The whitespace difference itself is reported as a finding rather
// than being treated as approved: nothing here approves it, and it is in
// the report handed back with this change, because it also breaks §5.5's
// own rollout plan -- "divergence is then a diff of two heartbeat objects"
// is not a usable diff when one side is pretty-printed and the other is
// not.
//
// An object that is NOT JSON on both sides -- applied/HEAD, a plan digest
// -- is compared byte for byte, with no exceptions at all. Those are the
// two objects whose exact bytes the system depends on.
func compareObject(key, bash, truss string) []Diff {
	bj, bok := flatten(sortExpiring(bash))
	tj, tok := flatten(sortExpiring(truss))
	if !bok || !tok {
		if bash != truss {
			return []Diff{{Kind: "value", Key: key, Bash: bash, Truss: truss}}
		}
		return nil
	}

	paths := map[string]bool{}
	for p := range bj {
		paths[p] = true
	}
	for p := range tj {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	var diffs []Diff
	for _, p := range sorted {
		if timestampPaths[p] {
			continue
		}
		bv, inBash := bj[p]
		tv, inTruss := tj[p]
		if inBash && inTruss && bv == tv {
			continue
		}
		if dayCountPath.MatchString(p) && inBash && inTruss && withinDays(bv, tv, dayCountTolerance) {
			continue
		}
		diffs = append(diffs, Diff{Kind: "value", Key: key, Path: p, Bash: present(bv, inBash), Truss: present(tv, inTruss)})
	}
	return diffs
}

func present(v string, ok bool) string {
	if !ok {
		return "<absent>"
	}
	return v
}

func withinDays(a, b string, tolerance int) bool {
	x, err1 := strconv.ParseFloat(a, 64)
	y, err2 := strconv.ParseFloat(b, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	return math.Abs(x-y) <= float64(tolerance)
}

// sortExpiring orders a heartbeat's `expiring` array by name.
//
// ⚠️ THIS IS A THIRD EXCEPTION, BEYOND THE TWO §5.5 DOCUMENTS, AND IT IS
// STATED HERE RATHER THAN HIDDEN IN A DIVERGENCE ENTRY. Neither
// implementation's order is specified by anything: the bash's is whatever
// `op item list` happened to return, and truss's is a Cloudflare probe
// followed by Vault's LIST, which Vault sorts. Nothing reads a position in
// this array -- the heartbeat's consumers read names, and the alert joins
// them into prose. Left unsorted, one extra credential shifts every later
// index and turns a one-line membership difference into a diff per field,
// which buries the thing that actually matters (WHICH credentials each side
// reports) under noise about where they sit. The membership difference is
// still compared, exactly, and is still enumerated in divergences.go.
//
// It is deliberately narrow: only the top-level `expiring` array, only in
// an object that has one, and only by name. Nothing else in either
// implementation's output is reordered before comparison.
func sortExpiring(body string) string {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return body
	}
	raw, ok := doc["expiring"]
	if !ok {
		return body
	}
	var items []struct {
		Name     string `json:"name"`
		DaysLeft *int   `json:"days_left"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return body
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	sorted, err := json.Marshal(items)
	if err != nil {
		return body
	}
	doc["expiring"] = sorted
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return string(out)
}

// flatten turns a JSON document into path -> scalar, so a disagreement is
// reported at the field that disagrees rather than as two large blobs. It
// reports ok=false for anything that is not JSON.
func flatten(s string) (map[string]string, bool) {
	var doc any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		return nil, false
	}
	out := map[string]string{}
	walk("", doc, out)
	return out, true
}

func walk(path string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			out[path] = "{}"
			return
		}
		for k, child := range t {
			walk(path+"/"+k, child, out)
		}
	case []any:
		if len(t) == 0 {
			out[path] = "[]"
			return
		}
		for i, child := range t {
			walk(path+"/"+strconv.Itoa(i), child, out)
		}
	case nil:
		out[path] = "null"
	case bool:
		out[path] = strconv.FormatBool(t)
	case float64:
		out[path] = strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		out[path] = t
	default:
		out[path] = fmt.Sprint(t)
	}
}

// Unexplained returns the diffs no allowlist entry accounts for, and the
// IDs of the entries that did account for one.
//
// ⚠️ THE SECOND RETURN IS AS LOAD-BEARING AS THE FIRST. An allowlist that
// keeps entries for divergences that no longer happen is an allowlist that
// would silently accept them if they came back, and it is how a list like
// this rots into a list of excuses. The caller unions these across every
// scenario and fails on an entry that matched nothing anywhere -- the same
// standing rule this repo already applies to a check nobody has watched
// fail.
func Unexplained(scenario string, diffs []Diff, list []Divergence) (unexplained []Diff, used []string) {
	for _, d := range diffs {
		matched := false
		for _, entry := range list {
			if entry.Matches(scenario, d) {
				used = append(used, entry.ID)
				matched = true
				break
			}
		}
		if !matched {
			unexplained = append(unexplained, d)
		}
	}
	return unexplained, used
}

// Describe renders diffs for a test failure message.
func Describe(diffs []Diff) string {
	var b strings.Builder
	for _, d := range diffs {
		b.WriteString("\n  - ")
		b.WriteString(d.String())
	}
	return b.String()
}
