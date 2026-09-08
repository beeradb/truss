// Package plan canonicalises an OpenTofu `show -json` plan into the exact
// bytes the reference pipeline produces, and hashes them.
//
//	jq -S -c '<the filter in Canonical's doc comment>' | sha256sum | cut -d" " -f1
//
// ⚠️ THIS IS THE ONE BYTE-IDENTICAL REQUIREMENT IN THE SYSTEM. Every digest
// already recorded in the ledger was computed by that jq pipeline, at every
// head sha whose commit has not yet applied. A single byte of divergence
// here invalidates all of them, silently, as refusals that look like
// tampering rather than as a bug in this package. See docs/port-plan.md
// §2 item 1 and §4.3a.
//
// encoding/json's Marshal cannot produce these bytes, and this package does
// not use it to write output. Measured against jq 1.7: it escapes <, >, &
// unless told not to; it escapes U+2028/U+2029, which jq never does; it does
// not escape U+007F, which jq does; and it re-renders every number through
// float64, which does not preserve a source literal like "2.50", "1.0" or a
// 39-digit integer, and collapses "1e3" and "1000" to the same text where jq
// keeps them apart. This package parses numbers as literal (sign, digits,
// scale) triples using math/big only for the arithmetic on the exponent —
// never a float64 — and writes its own compact-JSON emitter.
package plan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
)

// theFilter is the reference bash's own jq program, reproduced verbatim so
// a diff against applier/plan-digest is a diff against this constant.
const theFilter = `[ (.resource_changes // [])[] | { address, actions: .change.actions, before: .change.before, after: .change.after } ] | sort_by(.address)`

// Canonical returns the exact bytes that
//
//	jq -S -c '[ (.resource_changes // [])[] | { address, actions: .change.actions, before: .change.before, after: .change.after } ] | sort_by(.address)'
//
// writes for planJSON: resource_changes only — for each element
// {address, actions, before, after} built from .address, .change.actions,
// .change.before, .change.after — sorted by address with a stable sort,
// every object's keys sorted recursively by byte order, compact, no
// whitespace other than the single trailing newline jq itself always
// writes after a -c value. That newline is part of what sha256sum hashes
// in the reference pipeline, so it is part of what Canonical returns too:
// Canonical is exported precisely so a mismatch is a byte diff, not two
// unequal hashes with nothing to compare them against.
//
// Nothing outside resource_changes ever reaches the output: not timestamp,
// prior_state, configuration, provider metadata, nor any key of a
// resource_changes element or its change object other than the four named
// above. Those are exactly the things that move between two runs of one
// plan for reasons that are not a change to infrastructure.
func Canonical(planJSON []byte) ([]byte, error) {
	root, err := decodeOne(planJSON)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	changes, err := resourceChanges(root)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	type item struct {
		addr  interface{}
		entry interface{}
	}
	items := make([]item, len(changes))
	for i, rc := range changes {
		entry, addr, err := buildEntry(rc)
		if err != nil {
			return nil, fmt.Errorf("plan: resource_changes[%d] %s", i, err)
		}
		items[i] = item{addr: addr, entry: entry}
	}

	// A stable sort, deliberately: two resource_changes elements sharing an
	// address (jq's sort_by is not documented to be stable across versions)
	// must keep their input order, or two runs of the filter over the same
	// plan could disagree with each other.
	sort.SliceStable(items, func(i, j int) bool {
		return compareValues(items[i].addr, items[j].addr) < 0
	})

	arr := make([]interface{}, len(items))
	for i, it := range items {
		arr[i] = it.entry
	}

	var buf bytes.Buffer
	if err := writeValue(&buf, arr); err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// Digest is sha256(Canonical(planJSON)), 64 lowercase hex characters, no
// trailing newline — the same string `sha256sum | cut -d" " -f1` prints.
func Digest(planJSON []byte) (string, error) {
	canon, err := Canonical(planJSON)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

// decodeOne parses exactly one JSON value, preserving number literals
// (json.Number, never float64), and refuses anything left over but
// whitespace — jq itself would treat a second top-level value as a second
// program run, which is not a shape a plan file has.
func decodeOne(data []byte) (interface{}, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var extra json.RawMessage
	switch err := dec.Decode(&extra); err {
	case io.EOF:
		// nothing left but whitespace, as expected
	case nil:
		return nil, errors.New("invalid JSON: trailing data after the top-level value")
	default:
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}

	// encoding/json is more lenient than jq here: a \uXXXX escape naming an
	// unpaired UTF-16 high surrogate decodes silently to U+FFFD instead of
	// being refused. jq's parser refuses the WHOLE document for it — found
	// by FuzzCanonicalAgreesWithJQ with `{"": [{"": {"":"\ud800"}}]}`, where
	// jq exits 5 ("Invalid \uXXXX\uXXXX surrogate pair escape") and the
	// standard library decodes happily. Left alone, that would mean a
	// document the reference pipeline refuses outright could still produce
	// *some* digest here — the one failure mode "malformed JSON is refused,
	// never silently digested" cannot afford. A lone unpaired LOW surrogate
	// is not refused by jq (verified: it becomes one U+FFFD, byte-identical
	// to what encoding/json already produces), so only the high-surrogate
	// case needs this extra pass.
	if err := refuseUnpairedHighSurrogates(data); err != nil {
		return nil, err
	}
	return v, nil
}

// refuseUnpairedHighSurrogates re-scans the raw text — already known to be
// syntactically valid JSON by this point — for a \uXXXX high-surrogate
// escape (U+D800-U+DBFF) not immediately followed by a \uXXXX low-surrogate
// escape (U+DC00-U+DFFF). Byte-level scanning for the ASCII '"' and '\\' is
// safe here even though the text may carry UTF-8 multi-byte sequences,
// because every continuation and lead byte of a multi-byte sequence is
// >= 0x80 and can never equal either of those two ASCII bytes.
func refuseUnpairedHighSurrogates(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(data) {
				return nil // already-valid JSON: unreachable
			}
			if data[i+1] != 'u' {
				i++ // \" \\ \/ \b \f \n \r \t: one escaped byte
				continue
			}
			if i+6 > len(data) {
				return nil // unreachable in already-valid JSON
			}
			unit, ok := parseHex4(data[i+2 : i+6])
			if !ok {
				return nil // unreachable in already-valid JSON
			}
			if unit >= 0xD800 && unit <= 0xDBFF {
				if i+12 <= len(data) && data[i+6] == '\\' && data[i+7] == 'u' {
					if low, ok := parseHex4(data[i+8 : i+12]); ok && low >= 0xDC00 && low <= 0xDFFF {
						i += 11 // consumed both \uXXXX\uXXXX; loop's i++ covers the last byte
						continue
					}
				}
				return fmt.Errorf("invalid JSON: unpaired UTF-16 high surrogate escape")
			}
			i += 5 // consumed \uXXXX; loop's i++ covers the last byte
		}
	}
	return nil
}

func parseHex4(b []byte) (rune, bool) {
	if len(b) != 4 {
		return 0, false
	}
	var v rune
	for _, c := range b {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			v |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			v |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return v, true
}

// resourceChanges is `.resource_changes // []` over root, exactly as jq
// evaluates it: indexing null always yields null rather than an error, so a
// null plan is a plan with no changes; indexing anything else that is not
// an object is an error, matching jq's "Cannot index <type> with string".
// `//` fires on both null and false, so a resource_changes field that is
// present but false also yields the empty list — jq's operator, not this
// package's invention, and worth stating because the spec text alone says
// only "absent or null".
func resourceChanges(root interface{}) ([]interface{}, error) {
	if root == nil {
		return nil, nil
	}
	obj, ok := root.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("cannot index %s with \"resource_changes\"", typeName(root))
	}
	rc, present := obj["resource_changes"]
	if !present || isFalsy(rc) {
		return nil, nil
	}
	arr, ok := rc.([]interface{})
	if !ok {
		return nil, fmt.Errorf("resource_changes is a %s, not an array", typeName(rc))
	}
	return arr, nil
}

// buildEntry turns one element of resource_changes into the canonical
// {address, actions, before, after} object, plus the address value the
// caller sorts by.
//
// A null element is not an error — jq's `.address` and `.change.actions` on
// null both yield null, the same as a present `"change": null` or an absent
// change key — because indexing null never errors in jq, only indexing a
// non-null non-object does. Every other non-object element is refused.
func buildEntry(rc interface{}) (entry interface{}, addr interface{}, err error) {
	if rc == nil {
		return map[string]interface{}{"address": nil, "actions": nil, "before": nil, "after": nil}, nil, nil
	}
	obj, ok := rc.(map[string]interface{})
	if !ok {
		return nil, nil, fmt.Errorf("is a %s, not an object", typeName(rc))
	}
	addr = obj["address"]

	var actions, before, after interface{}
	if ch, present := obj["change"]; present && !isFalsy2(ch) {
		chObj, ok := ch.(map[string]interface{})
		if !ok {
			return nil, nil, fmt.Errorf(".change is a %s, not an object", typeName(ch))
		}
		actions = chObj["actions"]
		before = chObj["before"]
		after = chObj["after"]
	}

	entry = map[string]interface{}{
		"address": addr,
		"actions": actions,
		"before":  before,
		"after":   after,
	}
	return entry, addr, nil
}

// isFalsy2 reports whether ch is exactly JSON null. Unlike resourceChanges'
// use of `//`, `.change.actions` is a plain index, which only treats null
// specially (never false) — a change value of `false` is not an object and
// is refused, matching jq's "Cannot index boolean with string".
func isFalsy2(v interface{}) bool { return v == nil }

func isFalsy(v interface{}) bool {
	if v == nil {
		return true
	}
	b, ok := v.(bool)
	return ok && !b
}

func typeName(v interface{}) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// ---- ordering, exactly as jq's generic value comparison does it ----

// typeRank is jq's own type order: null < false < true < numbers < strings
// < arrays < objects.
func typeRank(v interface{}) int {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if !x {
			return 1
		}
		return 2
	case json.Number:
		return 3
	case string:
		return 4
	case []interface{}:
		return 5
	case map[string]interface{}:
		return 6
	default:
		panic(fmt.Sprintf("plan: value of unexpected decoded type %T", v))
	}
}

func compareValues(a, b interface{}) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		if ra < rb {
			return -1
		}
		return 1
	}
	switch ra {
	case 0, 1, 2: // null, false, true: exactly one value each
		return 0
	case 3:
		return compareDecimals(parseDecimal(string(a.(json.Number))), parseDecimal(string(b.(json.Number))))
	case 4:
		return strings.Compare(a.(string), b.(string))
	case 5:
		return compareArrays(a.([]interface{}), b.([]interface{}))
	default:
		return compareObjects(a.(map[string]interface{}), b.(map[string]interface{}))
	}
}

func compareArrays(a, b []interface{}) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := compareValues(a[i], b[i]); c != 0 {
			return c
		}
	}
	return compareInts(len(a), len(b))
}

// compareObjects is jq's own algorithm: compare the two objects' sorted key
// lists first (lexicographically, shorter-is-smaller-when-a-prefix), and
// only if those are equal compare the values in that shared key order.
// Verified against real jq — this is not documented anywhere jq ships, only
// observable — with {"a":1} < {"a":1,"b":2} < {"a":1,"c":0} < {"b":1}.
func compareObjects(a, b map[string]interface{}) int {
	ak, bk := sortedKeys(a), sortedKeys(b)
	if c := compareStringSlices(ak, bk); c != 0 {
		return c
	}
	for _, k := range ak {
		if c := compareValues(a[k], b[k]); c != 0 {
			return c
		}
	}
	return 0
}

func compareStringSlices(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := strings.Compare(a[i], b[i]); c != 0 {
			return c
		}
	}
	return compareInts(len(a), len(b))
}

func compareInts(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- numbers: parsed and compared as exact decimals, never as float64 ----

// decimal is a JSON number literal's sign, significant digits and scale,
// exactly as decNumber (and so jq 1.7) would hold it: value = coeff *
// 10^-scale. coeff carries no leading zeros ("0" for a zero value); scale
// is a *big.Int because a JSON exponent is syntactically unbounded and nothing
// here converts through a fixed-width integer to find out whether it was.
type decimal struct {
	neg   bool
	coeff string
	scale *big.Int
}

// parseDecimal reads a JSON number token — already validated as such by
// encoding/json's scanner — into its (sign, digits, scale) triple. It never
// fails: every input reaching it is grammar-conformant by construction.
func parseDecimal(lit string) decimal {
	s := lit
	neg := false
	if len(s) > 0 && s[0] == '-' {
		neg = true
		s = s[1:]
	}

	mantissa := s
	expDigits := ""
	expNeg := false
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mantissa = s[:i]
		rest := s[i+1:]
		if len(rest) > 0 && (rest[0] == '+' || rest[0] == '-') {
			expNeg = rest[0] == '-'
			rest = rest[1:]
		}
		expDigits = rest
	}

	intPart := mantissa
	fracPart := ""
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		intPart = mantissa[:i]
		fracPart = mantissa[i+1:]
	}

	raw := intPart + fracPart
	coeff := strings.TrimLeft(raw, "0")
	if coeff == "" {
		coeff = "0"
	}

	scale := big.NewInt(int64(len(fracPart)))
	if expDigits != "" {
		e := new(big.Int)
		e.SetString(expDigits, 10) // pure digits: SetString cannot fail here
		if expNeg {
			scale.Add(scale, e)
		} else {
			scale.Sub(scale, e)
		}
	}

	return decimal{neg: neg, coeff: coeff, scale: scale}
}

// adjustedExponent is decNumber's own term: (precision - 1) - scale, the
// power of ten of the leading digit. It decides plain-vs-scientific layout
// and, since a coefficient with no leading zero is confined to
// [10^adjustedExponent, 10^(adjustedExponent+1)), it also decides magnitude
// order between two decimals whose adjusted exponents differ, exactly and
// without needing to align their scales.
func adjustedExponent(d decimal) *big.Int {
	p := big.NewInt(int64(len(d.coeff) - 1))
	return p.Sub(p, d.scale)
}

var minusSix = big.NewInt(-6)

// formatDecimal renders d the way decNumber's to-scientific-string does —
// which is to say, the way Java's BigDecimal.toString() does, which is what
// jq 1.7 uses. Verified against real jq for every case in
// docs/port-plan.md §4.3a plus the boundary cases in digest_test.go: plain
// notation when the scale is non-negative and the adjusted exponent is at
// least -6, scientific otherwise.
func formatDecimal(d decimal) string {
	precision := len(d.coeff)
	adjExp := adjustedExponent(d)
	usePlain := d.scale.Sign() >= 0 && adjExp.Cmp(minusSix) >= 0

	var body string
	if usePlain {
		// usePlain implies scale <= precision+5 (from adjExp >= -6), so this
		// always fits comfortably in an int regardless of how large the
		// scale's own magnitude might otherwise be.
		scaleInt := int(d.scale.Int64())
		switch {
		case scaleInt == 0:
			body = d.coeff
		case len(d.coeff) > scaleInt:
			body = d.coeff[:len(d.coeff)-scaleInt] + "." + d.coeff[len(d.coeff)-scaleInt:]
		default:
			body = "0." + strings.Repeat("0", scaleInt-len(d.coeff)) + d.coeff
		}
	} else {
		if precision == 1 {
			body = d.coeff
		} else {
			body = d.coeff[:1] + "." + d.coeff[1:]
		}
		expText := adjExp.String()
		if adjExp.Sign() >= 0 {
			expText = "+" + expText
		}
		body += "E" + expText
	}

	if d.neg {
		return "-" + body
	}
	return body
}

// compareDecimals orders two decimals by exact value, never by converting
// either through float64. Zero is compared by sign alone (coeff "0" is
// zero regardless of scale). Same-sign nonzero decimals are ordered first
// by adjustedExponent, which is exact and needs no common scale; only a tie
// there needs the digits themselves, and even then no scale alignment is
// needed — padding the shorter coefficient with trailing zeros to the
// longer's length is exact once the two are known to share a magnitude
// class.
func compareDecimals(a, b decimal) int {
	az, bz := a.coeff == "0", b.coeff == "0"
	switch {
	case az && bz:
		return 0
	case az:
		if b.neg {
			return 1
		}
		return -1
	case bz:
		if a.neg {
			return -1
		}
		return 1
	}
	if a.neg != b.neg {
		if a.neg {
			return -1
		}
		return 1
	}
	c := compareMagnitude(a, b)
	if a.neg {
		return -c
	}
	return c
}

func compareMagnitude(a, b decimal) int {
	if c := adjustedExponent(a).Cmp(adjustedExponent(b)); c != 0 {
		return c
	}
	n := len(a.coeff)
	if len(b.coeff) > n {
		n = len(b.coeff)
	}
	ca := a.coeff + strings.Repeat("0", n-len(a.coeff))
	cb := b.coeff + strings.Repeat("0", n-len(b.coeff))
	return strings.Compare(ca, cb)
}

// ---- the emitter: compact JSON, jq's own escaping, no encoding/json ----

func writeValue(buf *bytes.Buffer, v interface{}) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		buf.WriteString(formatDecimal(parseDecimal(string(x))))
	case string:
		writeString(buf, x)
	case []interface{}:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]interface{}:
		buf.WriteByte('{')
		for i, k := range sortedKeys(x) {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := writeValue(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unexpected value of type %T", v)
	}
	return nil
}

// writeString escapes exactly what jq escapes and nothing more: `"`, `\`,
// the six short forms, every other code point below 0x20, and U+007F —
// each as a lowercase \u00xx. Everything else, including <, >, &, /,
// U+2028 and U+2029, is written as raw UTF-8. Verified against real jq
// with all of the above in one string.
func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}
