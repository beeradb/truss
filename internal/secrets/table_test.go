package secrets

import (
	"strings"
	"testing"
)

func TestAnEmptyTableIsRefused(t *testing.T) {
	// Checking the message, not just non-nil, matters here: an empty
	// reader is also invalid JSON, so a version of this function with no
	// explicit empty check at all would still fail (on the JSON parse) and
	// a looser assertion would not catch its absence. The message this
	// function actually gives for a genuinely empty file names emptiness,
	// not a parse failure.
	t.Run("zero bytes", func(t *testing.T) {
		_, err := LoadExpiries(strings.NewReader(""), nil)
		if err == nil {
			t.Fatal("LoadExpiries on an empty reader = nil error, want a refusal")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("error %q does not say the table is empty", err.Error())
		}
	})
	t.Run("whitespace only", func(t *testing.T) {
		_, err := LoadExpiries(strings.NewReader("   \n\t  "), nil)
		if err == nil {
			t.Fatal("LoadExpiries on a whitespace-only reader = nil error, want a refusal")
		}
		if !strings.Contains(err.Error(), "empty") {
			t.Errorf("error %q does not say the table is empty", err.Error())
		}
	})
	t.Run("valid JSON but no entries", func(t *testing.T) {
		_, err := LoadExpiries(strings.NewReader("{}"), nil)
		if err == nil {
			t.Fatal("LoadExpiries on {} = nil error, want a refusal -- no entries is the same failure as no file")
		}
		if !strings.Contains(err.Error(), "no items") {
			t.Errorf("error %q does not say the table lists no items", err.Error())
		}
	})
}

func TestAnUnparseableExpiryIsRefused(t *testing.T) {
	raw := `{"github-app": {"expires": "not-a-real-date", "why": "testing"}}`
	_, err := LoadExpiries(strings.NewReader(raw), nil)
	if err == nil {
		t.Fatal("LoadExpiries with an unparseable expires value = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "github-app") {
		t.Errorf("error %q does not name the offending item", err.Error())
	}
}

func TestATableEntryForAProbedItemIsRefused(t *testing.T) {
	raw := `{
		"cf-token-mint": {"expires": "2027-01-01", "why": "should never be here"},
		"github-app": {"expires": "2027-01-01", "why": "fine"}
	}`
	_, err := LoadExpiries(strings.NewReader(raw), []string{"cf-token-mint"})
	if err == nil {
		t.Fatal("LoadExpiries with an entry naming a probed item = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "cf-token-mint") {
		t.Errorf("error %q does not name the probed item", err.Error())
	}
}

func TestATableWhereEveryEntryIsNeverIsRefused(t *testing.T) {
	raw := `{
		"github-app": {"expires": "never", "why": "app keys never expire"},
		"telegram-alert": {"expires": "never", "why": "bot tokens never expire"}
	}`
	_, err := LoadExpiries(strings.NewReader(raw), nil)
	if err == nil {
		t.Fatal("LoadExpiries with every entry \"never\" = nil error, want a refusal -- that silences the alarm forever")
	}
}

func TestATableWithAtLeastOneRealDateAlongsideNeverIsAccepted(t *testing.T) {
	raw := `{
		"github-app": {"expires": "never", "why": "app keys never expire"},
		"gcp-apply": {"expires": "2027-01-01", "why": "hand-made SA key, no validBeforeTime"}
	}`
	tbl, err := LoadExpiries(strings.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("LoadExpiries with one real date and one never = %v, want no error", err)
	}
	if tbl["github-app"] != "never" || tbl["gcp-apply"] != "2027-01-01" {
		t.Errorf("table = %+v, want both entries carried through", tbl)
	}
}

// TestAnAbsentItemGetsNoWriteAndIsNotDefaultedToNever is the load-bearing
// distinction the whole design turns on: an item this table never mentions
// must read as "no write", via the ordinary Go map idiom, never as an
// implicit "never".
func TestAnAbsentItemGetsNoWriteAndIsNotDefaultedToNever(t *testing.T) {
	raw := `{"github-app": {"expires": "2027-01-01", "why": "fine"}}`
	tbl, err := LoadExpiries(strings.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("LoadExpiries: %v", err)
	}

	v, ok := tbl["cf-infra-admin"]
	if ok {
		t.Fatalf("tbl[%q] = (%q, true), want ok=false -- an absent item must never be defaulted to any value, including \"never\"", "cf-infra-admin", v)
	}
	if v != "" {
		t.Errorf("tbl[%q] = %q for a missing key, want the zero value", "cf-infra-admin", v)
	}
}

func TestLoadExpiriesRejectsInvalidJSON(t *testing.T) {
	if _, err := LoadExpiries(strings.NewReader("not json at all"), nil); err == nil {
		t.Fatal("LoadExpiries with invalid JSON = nil error, want a refusal")
	}
}
