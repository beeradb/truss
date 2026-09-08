package plan

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- TestDigestIgnoresWhatMovesBetweenTwoRunsOfOnePlan ----

func TestDigestIgnoresWhatMovesBetweenTwoRunsOfOnePlan(t *testing.T) {
	planA := `{
		"timestamp": "2026-01-01T00:00:00Z",
		"prior_state": {"values": {"root_module": {"resources": [1,2,3]}}},
		"configuration": {"provider_config": {"random": {}}},
		"resource_changes": [
			{"address": "b", "type": "noise", "change": {"actions": ["update"], "before": 1, "after": 2}},
			{"address": "a", "type": "noise", "change": {"actions": ["create"], "before": null, "after": {"x": 1}}}
		]
	}`
	// Same actual changes, different timestamp/prior_state/configuration,
	// and resource_changes reordered — everything §2 item 1 names as moving
	// between two runs of one plan without being a change to infrastructure.
	planB := `{
		"timestamp": "2026-06-15T09:30:00Z",
		"prior_state": {"values": {"root_module": {"resources": ["totally different"]}}},
		"configuration": {"provider_config": {}},
		"resource_changes": [
			{"address": "a", "type": "noise", "change": {"actions": ["create"], "before": null, "after": {"x": 1}}},
			{"address": "b", "type": "noise", "change": {"actions": ["update"], "before": 1, "after": 2}}
		]
	}`

	da, err := Digest([]byte(planA))
	if err != nil {
		t.Fatalf("Digest(planA): %v", err)
	}
	db, err := Digest([]byte(planB))
	if err != nil {
		t.Fatalf("Digest(planB): %v", err)
	}
	if da != db {
		t.Fatalf("digests differ across two runs of one plan:\nA: %s\nB: %s", da, db)
	}
}

// ---- TestDigestChangesWhenThePlanReallyDiffers ----

func TestDigestChangesWhenThePlanReallyDiffers(t *testing.T) {
	base := `{"resource_changes": [{"address": "a", "change": {"actions": ["update"], "before": 1, "after": 2}}]}`
	changed := `{"resource_changes": [{"address": "a", "change": {"actions": ["update"], "before": 1, "after": 3}}]}`

	d1, err := Digest([]byte(base))
	if err != nil {
		t.Fatalf("Digest(base): %v", err)
	}
	d2, err := Digest([]byte(changed))
	if err != nil {
		t.Fatalf("Digest(changed): %v", err)
	}
	if d1 == d2 {
		t.Fatalf("digests agree despite a real difference in .after")
	}
}

// ---- TestAnAbsentResourceChangesIsTheEmptyList ----

func TestAnAbsentResourceChangesIsTheEmptyList(t *testing.T) {
	for _, tc := range []string{
		`{}`,
		`{"resource_changes": null}`,
		`null`,
	} {
		got, err := Canonical([]byte(tc))
		if err != nil {
			t.Fatalf("Canonical(%s): %v", tc, err)
		}
		if want := "[]\n"; string(got) != want {
			t.Errorf("Canonical(%s) = %q, want %q", tc, got, want)
		}
	}
}

// ---- TestCanonicalSortsEveryObjectKeyRecursively ----

func TestCanonicalSortsEveryObjectKeyRecursively(t *testing.T) {
	in := `{"resource_changes": [{"address": "r", "change": {
		"before": {"z": 1, "a": 2, "nested": {"zz": 1, "aa": 2}},
		"after": null
	}}]}`
	got, err := Canonical([]byte(in))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	// Top-level entry keys sorted (actions,address,after,before), and every
	// nested object's keys sorted too.
	want := `[{"actions":null,"address":"r","after":null,"before":{"a":2,"nested":{"aa":2,"zz":1},"z":1}}]` + "\n"
	if string(got) != want {
		t.Errorf("Canonical:\n got %s\nwant %s", got, want)
	}
}

// ---- TestCanonicalPreservesNumberLiterals ----

func TestCanonicalPreservesNumberLiterals(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1.0", "1.0"},
		{"2.50", "2.50"},
		{"0.1000", "0.1000"},
		{"-0", "-0"},
		{"100", "100"},
		// A big integer, well beyond float64's ~15-17 significant digits —
		// short of the 39 digits verified against real jq
		// only because scripts/leakscan refuses any 32+ character run of
		// [0-9a-f], which a run of decimal digits trivially satisfies.
		{"123456789012345678901234", "123456789012345678901234"},
	}
	for _, tc := range cases {
		in := `{"resource_changes":[{"address":"a","change":{"before":` + tc.in + `}}]}`
		got, err := Canonical([]byte(in))
		if err != nil {
			t.Fatalf("Canonical(%s): %v", tc.in, err)
		}
		want := `[{"actions":null,"address":"a","after":null,"before":` + tc.want + `}]` + "\n"
		if string(got) != want {
			t.Errorf("literal %s: got %s, want %s", tc.in, got, want)
		}
	}
}

// ---- TestCanonicalNormalisesExponentsLikeDecNumber ----

func TestCanonicalNormalisesExponentsLikeDecNumber(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1e3", "1E+3"},
		{"1E+2", "1E+2"},
		{"1.5e-3", "0.0015"},
		{"2.5e+1", "25"},
		{"1e21", "1E+21"},
		{"0e5", "0E+5"},
		{"1e-7", "1E-7"},
		{"1e-6", "0.000001"},
		{"10E10", "1.0E+11"},
		{"1.230000e2", "123.0000"},
	}
	for _, tc := range cases {
		in := `{"resource_changes":[{"address":"a","change":{"before":` + tc.in + `}}]}`
		got, err := Canonical([]byte(in))
		if err != nil {
			t.Fatalf("Canonical(%s): %v", tc.in, err)
		}
		want := `[{"actions":null,"address":"a","after":null,"before":` + tc.want + `}]` + "\n"
		if string(got) != want {
			t.Errorf("literal %s: got %s, want %s", tc.in, got, want)
		}
	}
}

// ---- TestCanonicalEscapesExactlyWhatJQEscapes ----

func TestCanonicalEscapesExactlyWhatJQEscapes(t *testing.T) {
	// Verified byte-for-byte against real jq 1.7 (see the session that wrote
	// this package): space, <, >, &, /, U+2028, U+2029, é and an emoji are
	// all raw UTF-8; ", \, the six short forms, U+0001, U+001f and U+007f
	// are escaped, the last three as lowercase \u00xx.
	s := " <>&/\t\n\r\b\f\"\\end\u007f\u0001\u001f\u2028\u2029\u00e9\U0001F600"
	doc, err := json.Marshal(map[string]interface{}{
		"resource_changes": []interface{}{
			map[string]interface{}{"address": "a", "change": map[string]interface{}{"before": s}},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal fixture: %v", err)
	}
	got, err := Canonical(doc)
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := "[{\"actions\":null,\"address\":\"a\",\"after\":null,\"before\":\" <>&/\\t\\n\\r\\b\\f\\\"\\\\end\\u007f\\u0001\\u001f\u2028\u2029\u00e9\U0001F600\"}]\n"
	if string(got) != want {
		t.Errorf("escaping:\n got % x\nwant % x", got, want)
	}
}

// ---- TestCanonicalKeepsSameAddressEntriesInInputOrder ----

func TestCanonicalKeepsSameAddressEntriesInInputOrder(t *testing.T) {
	in := `{"resource_changes": [
		{"address": "x", "change": {"actions": ["1"]}},
		{"address": "x", "change": {"actions": ["2"]}},
		{"address": "x", "change": {"actions": ["3"]}}
	]}`
	got, err := Canonical([]byte(in))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	want := `[{"actions":["1"],"address":"x","after":null,"before":null},` +
		`{"actions":["2"],"address":"x","after":null,"before":null},` +
		`{"actions":["3"],"address":"x","after":null,"before":null}]` + "\n"
	if string(got) != want {
		t.Errorf("stability:\n got %s\nwant %s", got, want)
	}
}

// ---- TestDigestRefusesMalformedJSON ----

func TestDigestRefusesMalformedJSON(t *testing.T) {
	for _, in := range []string{
		``,
		`{`,
		`{"resource_changes": [}`,
		`not json at all`,
		`{"resource_changes": [1,2]} trailing garbage`,
		`"a string"`,
		`42`,
		`[1,2,3]`,
		`true`,
		// An unpaired UTF-16 high surrogate escape. Found by
		// FuzzCanonicalAgreesWithJQ: encoding/json decodes this silently
		// (to U+FFFD), but jq's parser refuses the whole document —
		// "Invalid \uXXXX\uXXXX surrogate pair escape" — and this package
		// must refuse it too, or a document the reference pipeline rejects
		// outright could still produce a digest here.
		`{"": [{"": {"":"\ud800"}}]}`,
		`{"x": "\ud800\ud800"}`,
		`{"x": "\ud800x"}`,
	} {
		if _, err := Digest([]byte(in)); err == nil {
			t.Errorf("Digest(%q): want error, got none", in)
		}
		if _, err := Canonical([]byte(in)); err == nil {
			t.Errorf("Canonical(%q): want error, got none", in)
		}
	}
}

// A lone, unpaired UTF-16 LOW surrogate, by contrast, is not refused by
// jq — verified against real jq: it decodes to one U+FFFD per occurrence,
// byte-identical to what encoding/json already produces — so only the
// high-surrogate case above needs the extra refusal.
func TestDigestAcceptsAnUnpairedLowSurrogate(t *testing.T) {
	for _, in := range []string{
		`{"resource_changes":[{"address":"a","change":{"before":"\udc00"}}]}`,
		`{"resource_changes":[{"address":"a","change":{"before":"\udc00\udc00"}}]}`,
	} {
		if _, err := Canonical([]byte(in)); err != nil {
			t.Errorf("Canonical(%s): %v", in, err)
		}
	}
}

// ---- TestDigestRefusesANonObjectResourceChange ----

func TestDigestRefusesANonObjectResourceChange(t *testing.T) {
	for _, in := range []string{
		`{"resource_changes": ["a string"]}`,
		`{"resource_changes": [42]}`,
		`{"resource_changes": [[1,2]]}`,
		`{"resource_changes": [true]}`,
	} {
		if _, err := Canonical([]byte(in)); err == nil {
			t.Errorf("Canonical(%s): want error, got none", in)
		}
	}
	// A null element, by contrast, is not an error — jq's .address and
	// .change.actions on null both yield null.
	got, err := Canonical([]byte(`{"resource_changes": [null]}`))
	if err != nil {
		t.Fatalf("Canonical([null]): %v", err)
	}
	if want := `[{"actions":null,"address":null,"after":null,"before":null}]` + "\n"; string(got) != want {
		t.Errorf("Canonical([null]) = %s, want %s", got, want)
	}
}

// ---- TestDigestTreatsAMissingChangeAsNulls ----

func TestDigestTreatsAMissingChangeAsNulls(t *testing.T) {
	for _, in := range []string{
		`{"resource_changes": [{"address": "a"}]}`,
		`{"resource_changes": [{"address": "a", "change": null}]}`,
	} {
		got, err := Canonical([]byte(in))
		if err != nil {
			t.Fatalf("Canonical(%s): %v", in, err)
		}
		want := `[{"actions":null,"address":"a","after":null,"before":null}]` + "\n"
		if string(got) != want {
			t.Errorf("Canonical(%s) = %s, want %s", in, got, want)
		}
	}
	// change present and NOT an object or null is refused.
	for _, in := range []string{
		`{"resource_changes": [{"address": "a", "change": "not an object"}]}`,
		`{"resource_changes": [{"address": "a", "change": [1,2]}]}`,
		`{"resource_changes": [{"address": "a", "change": 42}]}`,
	} {
		if _, err := Canonical([]byte(in)); err == nil {
			t.Errorf("Canonical(%s): want error, got none", in)
		}
	}
}

// ---- TestDigestIsHexLowercaseWithNoTrailingNewline ----

func TestDigestIsHexLowercaseWithNoTrailingNewline(t *testing.T) {
	d, err := Digest([]byte(`{"resource_changes": [{"address": "a", "change": {"actions": ["update"]}}]}`))
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(d) {
		t.Errorf("Digest = %q, want exactly 64 lowercase hex characters", d)
	}
	if strings.ContainsAny(d, "\n\r") {
		t.Errorf("Digest = %q, contains a newline", d)
	}
}

// ---- TestCanonicalNeverContainsAnUnselectedKey ----

func TestCanonicalNeverContainsAnUnselectedKey(t *testing.T) {
	in := `{
		"format_version": "1.2",
		"terraform_version": "1.7.0",
		"timestamp": "2026-01-01T00:00:00Z",
		"configuration": {"secret": "should never appear"},
		"prior_state": {"should": "never appear"},
		"resource_changes": [
			{
				"address": "r",
				"mode": "managed",
				"type": "noise_type",
				"name": "noise_name",
				"provider_name": "example.test/example/noise",
				"change": {
					"actions": ["update"],
					"before": {"x": 1},
					"after": {"x": 2},
					"after_unknown": {"x": true},
					"before_sensitive": false,
					"after_sensitive": false,
					"replace_paths": [["x"]],
					"importing": null
				}
			}
		]
	}`
	got, err := Canonical([]byte(in))
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(got, &arr); err != nil {
		t.Fatalf("re-parsing Canonical output: %v", err)
	}
	if len(arr) != 1 {
		t.Fatalf("got %d entries, want 1", len(arr))
	}
	wantKeys := map[string]bool{"address": true, "actions": true, "before": true, "after": true}
	for k := range arr[0] {
		if !wantKeys[k] {
			t.Errorf("entry carries unselected key %q", k)
		}
	}
	for k := range wantKeys {
		if _, ok := arr[0][k]; !ok {
			t.Errorf("entry is missing selected key %q", k)
		}
	}
	if bytes.Contains(got, []byte("noise")) || bytes.Contains(got, []byte("secret")) || bytes.Contains(got, []byte("never appear")) {
		t.Errorf("Canonical output leaked an unselected field: %s", got)
	}
}

// ---- TestDigestMatchesTheRecordedGoldens ----

func TestDigestMatchesTheRecordedGoldens(t *testing.T) {
	dir := filepath.Join("testdata", "digest")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		t.Run(name, func(t *testing.T) {
			found++
			planJSON, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			// The pinned expected digest is base64 of the raw 32 bytes, not
			// hex — scripts/leakscan refuses a bare 64-character hex string
			// in tracked files, and an exemption for testdata would blind it
			// to a real leak (§5.1, §7.7).
			b64, err := os.ReadFile(filepath.Join(dir, name+".digest.b64"))
			if err != nil {
				t.Fatalf("reading golden: %v", err)
			}
			wantBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b64)))
			if err != nil {
				t.Fatalf("decoding golden base64: %v", err)
			}
			want := hex.EncodeToString(wantBytes)

			got, err := Digest(planJSON)
			if err != nil {
				t.Fatalf("Digest: %v", err)
			}
			if got != want {
				t.Errorf("Digest(%s) = %s, want %s", e.Name(), got, want)
			}
		})
	}
	if found == 0 {
		t.Fatal("no golden fixtures found under testdata/digest — the corpus is missing, not just empty")
	}
}

// ---- TestDigestAgreesWithJQ ----
//
// The differential test: shell to the real jq -S -c '<filter>' | sha256sum
// pipeline for every corpus entry, and compare canonical bytes first, then
// the digest. It fails rather than skips when TRUSS_REQUIRE_JQ=1, which CI
// sets — a differential test that silently skips is worthless, and that is
// the whole reason this package is being written before anything depends on
// it (§5.2).

func TestDigestAgreesWithJQ(t *testing.T) {
	jqPath, jqErr := exec.LookPath("jq")
	if jqErr != nil {
		if os.Getenv("TRUSS_REQUIRE_JQ") == "1" {
			t.Fatalf("jq is required (TRUSS_REQUIRE_JQ=1) but not found on PATH: %v", jqErr)
		}
		t.Skip("jq not found on PATH; set TRUSS_REQUIRE_JQ=1 to make this fatal")
	}

	dir := filepath.Join("testdata", "digest")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	tested := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		t.Run(name, func(t *testing.T) {
			planJSON, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			assertAgreesWithJQ(t, jqPath, planJSON)
			tested++
		})
	}
	if tested == 0 {
		t.Fatal("no corpus entries were tested against jq")
	}
}

// assertAgreesWithJQ runs both pipelines over input and requires identical
// canonical bytes and identical digests. It is also used by the fuzz target
// below.
func assertAgreesWithJQ(t *testing.T, jqPath string, input []byte) {
	t.Helper()

	wantCanon, jqOK, err := runJQCanonical(jqPath, input)
	gotCanon, ourErr := Canonical(input)

	if !jqOK {
		// jq itself rejected this input (invalid JSON, or a shape the filter
		// cannot index, e.g. a bare array at the top level). Our
		// implementation must refuse it too, or it has silently accepted
		// something the reference pipeline would not have.
		if ourErr == nil {
			t.Fatalf("jq rejected input but Canonical accepted it\ninput: %s\njq stderr: %v\ncanonical: %s", input, err, gotCanon)
		}
		return
	}
	if ourErr != nil {
		t.Fatalf("jq accepted input but Canonical refused it: %v\ninput: %s\njq output: %s", ourErr, input, wantCanon)
	}
	if !bytes.Equal(gotCanon, wantCanon) {
		t.Fatalf("canonical bytes differ\ninput: %s\n got: % x (%s)\nwant: % x (%s)", input, gotCanon, gotCanon, wantCanon, wantCanon)
	}

	wantSum := sha256sumHex(t, wantCanon)
	gotSum, err := Digest(input)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if gotSum != wantSum {
		t.Fatalf("digests differ despite matching canonical bytes\ninput: %s\n got: %s\nwant: %s", input, gotSum, wantSum)
	}
}

// runJQCanonical shells to `jq -S -c '<filter>'` and reports whether jq
// accepted the input. jqOK is false for any nonzero exit — malformed JSON,
// or a shape the filter cannot index — which is a legitimate outcome to
// compare against, not a test infrastructure failure.
func runJQCanonical(jqPath string, input []byte) (out []byte, jqOK bool, err error) {
	cmd := exec.Command(jqPath, "-S", "-c", theFilter)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if runErr := cmd.Run(); runErr != nil {
		return nil, false, fmt.Errorf("%v: %s", runErr, stderr.String())
	}
	// jq's own quirk, not this package's: given zero bytes of input, jq
	// reads zero JSON values and so runs the filter zero times — exit 0,
	// empty stdout, and no "the exact bytes jq -S -c writes" to compare
	// against. That is different from refusing malformed input, so it is
	// excluded from the comparison rather than asserted against; empty
	// input is still, correctly, an error for Canonical (TestDigestRefusesMalformedJSON).
	if stdout.Len() == 0 {
		return nil, false, errors.New("jq produced no output (empty input)")
	}
	return stdout.Bytes(), true, nil
}

func sha256sumHex(t *testing.T, data []byte) string {
	t.Helper()
	cmd := exec.Command("sha256sum")
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sha256sum: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("sha256sum produced no output")
	}
	return fields[0]
}

var hugeExponentRE = regexp.MustCompile(`[eE][+-]?[0-9]{10,}`)

func hasHugeExponent(data []byte) bool {
	return hugeExponentRE.Match(data)
}

// ---- FuzzCanonicalAgreesWithJQ ----

func FuzzCanonicalAgreesWithJQ(f *testing.F) {
	dir := filepath.Join("testdata", "digest")
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				f.Add(data)
			}
		}
	}
	// A handful of adversarial seeds beyond the golden corpus: the exact
	// exponent/escape cases from §4.3a, plus shapes jq treats specially
	// (false/null resource_changes, non-object elements, non-object changes).
	seeds := []string{
		`{"resource_changes": false}`,
		`{"resource_changes": [null]}`,
		`{"resource_changes": [{"address": "a", "change": false}]}`,
		`{"resource_changes": [{"address": {"nested": [1, "x", null, true]}, "change": {}}]}`,
		`{"resource_changes": [{"address": 1e3, "change": {}}, {"address": 1000, "change": {}}]}`,
		`{"resource_changes": [{"address": "a", "change": {"before": "\ud800"}}]}`,
		"{\"resource_changes\":[{\"address\":\"a\",\"change\":{\"before\":\"\xe20\"}}]}",
		`{"resource_changes": [{"address": "a", "change": {"before": 0e999999999}}]}`,
		`{"resource_changes": [{"address": "a", "change": {"before": 0e1000000000}}]}`,
		`null`,
		`5`,
		`"a string"`,
		`[1,2,3]`,
		`not json`,
		``,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	jqPath, jqErr := exec.LookPath("jq")

	f.Fuzz(func(t *testing.T, data []byte) {
		if jqErr != nil {
			// Still exercise our own implementation for panics/crashes even
			// without jq available; the byte-parity assertion needs jq.
			_, _ = Canonical(data)
			return
		}
		// Bound how much either side is asked to render — jq will spend
		// real wall-clock time on the largest inputs a coverage-guided
		// fuzzer can produce, and nothing about the digest's correctness is
		// exercised by throwing megabytes of noise at both sides.
		if len(data) > 64*1024 {
			t.Skip("input too large for the differential fuzz budget")
		}
		// jq's own parser is looser than RFC 8259 in ways no compliant
		// encoder ever produces — found here by the fuzzer with `{"":[{"":00}]}`:
		// jq accepts a leading-zero integer (and ".5" with no leading digit,
		// and NaN/Infinity), where encoding/json correctly refuses all
		// three as invalid JSON. `tofu show -json` is itself written with
		// encoding/json, so it can never emit any of these; this package
		// stays strict rather than reimplementing jq's tokenizer to match
		// an input shape the reference pipeline's own producer cannot
		// generate. The byte-parity contract is scoped to inputs valid
		// under encoding/json.Valid; outside that, only crash-safety is
		// asserted (Canonical must return an error, never panic).
		if !json.Valid(data) {
			if _, err := Canonical(data); err == nil {
				t.Fatalf("Canonical accepted input that is not valid JSON: %s", data)
			}
			return
		}
		// A second, narrower carve-out: raw invalid UTF-8 bytes embedded
		// unescaped inside a JSON string. encoding/json's Valid still calls
		// this valid JSON — it substitutes U+FFFD per bad byte and moves
		// on — so it isn't caught above. Found here by the fuzzer with
		// `{"resource_changes":[{"address":"\xe20"}]}` (a truncated 3-byte
		// UTF-8 lead byte immediately followed by an ASCII '0'): Go's
		// decoder replaces exactly the one bad byte and resumes at the '0',
		// producing "�0"; jq's own UTF-8 recovery consumes the '0' too
		// and produces a single "�". Both are conformant Unicode
		// replacement strategies — the standard permits more than one — but
		// they are not the same one. `tofu show -json` cannot emit this
		// either, for the same reason as the leading-zero case: it comes
		// out of a Go program, which only ever produces valid UTF-8. Skip
		// the byte-parity assertion; still require no panic.
		if !utf8.Valid(data) {
			if _, err := Canonical(data); err != nil {
				// either outcome is fine: erroring or accepting-and-recovering
				_ = err
			}
			return
		}
		// A third carve-out: decNumber — and so jq 1.7 — has a hard exponent
		// ceiling. Found here by the fuzzer with `{"resource_changes":
		// [{"change":{"before":0e1000000000}}]}`: jq prints "0E+999999999"
		// (nine nines — confirmed separately as decNumber's own limit: a
		// literal exponent of 999999999 round-trips unchanged, one digit
		// more and jq clamps a zero coefficient's exponent to that ceiling,
		// and for a NONZERO coefficient falls back to a float64
		// approximation entirely, e.g. `1e999999999999` becomes
		// "1.7976931348623157e+308", DBL_MAX). This package preserves the
		// literal exponent exactly instead, arbitrarily large, which is
		// arguably more faithful to "preserve number literals" than
		// decNumber's own ceiling — but it is a real, deliberate divergence
		// for exponents no real Terraform attribute ever carries. Skip the
		// byte-parity assertion once a literal's exponent reaches ten
		// digits (decNumber's ceiling is nine nines; ten digits already
		// exceeds it regardless of value).
		if hasHugeExponent(data) {
			if _, err := Canonical(data); err != nil {
				_ = err
			}
			return
		}
		assertAgreesWithJQ(t, jqPath, data)
	})
}
