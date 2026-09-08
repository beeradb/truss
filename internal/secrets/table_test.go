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

// ⚠️ AN ALL-"never" TABLE IS ACCEPTED, AND THAT REPLACED THE OPPOSITE RULE.
//
// The earlier rule refused any table in which every entry was "never",
// reasoning that an all-"never" table silences the daily alarm forever. Right
// danger, wrong signal: EVERY hand-made credential in this system genuinely
// never expires -- a GitHub App private key has no expiry, a Telegram bot
// token has none, a passphrase is not an issued credential -- so the honest
// table IS all-"never", and the rule refused the truth. Deploying it proved
// that: the publisher crash-looped on the real table, and because it is a
// native sidecar the whole daily pass could not start.
//
// What protects the alarm now is that a "never" cannot be set SILENTLY.
func TestATableWhereEveryEntryIsNeverIsAcceptedWhenEachSaysWhy(t *testing.T) {
	raw := `{
		"github-app": {"expires": "never", "why": "a GitHub App private key has no expiry; the tokens minted from it live an hour and are never stored"},
		"telegram-alert": {"expires": "never", "why": "a Telegram bot token does not expire; it is revoked by regenerating it in BotFather"}
	}`
	got, err := LoadExpiries(strings.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("LoadExpiries on an honest all-\"never\" table = %v, want it accepted", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
}

// TestANeverWithNoWhyIsRefused is the guard that replaced it, and it is the
// one that matters: a credential exempted from the expiry alarm has to say
// what makes it permanent, in the file, where a reviewer reads it.
func TestANeverWithNoWhyIsRefused(t *testing.T) {
	for _, why := range []string{"", "   ", "\\t \\n"} {
		raw := `{
			"github-app": {"expires": "never", "why": "` + why + `"},
			"telegram-alert": {"expires": "never", "why": "bot tokens do not expire"}
		}`
		_, err := LoadExpiries(strings.NewReader(raw), nil)
		if err == nil {
			t.Fatalf("LoadExpiries accepted a \"never\" whose why was %q -- an unjustified never is exactly what silences the alarm", why)
		}
		if !strings.Contains(err.Error(), "github-app") {
			t.Errorf("the refusal does not name the offending item: %v", err)
		}
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

// ⚠️ THE DEPLOYED TABLE ITSELF IS NOT TESTED HERE, AND THAT IS A REAL GAP.
//
// The file the ConfigMap ships lives in the platform repo, so a test in this
// repo could only reach it by a relative path across checkouts -- which passes
// locally, SKIPS in CI, and is therefore the vacuous check this project has a
// standing rule against. One was written and deleted rather than kept.
//
// What actually caught the first broken table was the publisher failing closed
// in the cluster: it crash-looped, and because it is a native sidecar the whole
// daily pass could not start. That is the right failure DIRECTION and the wrong
// PLACE to discover it -- pod Pending at 04:10, with concurrencyPolicy Forbid
// suppressing every later pass.
//
// The gap is closed at deploy time, not here: see the platform repo's
// applier/README for the step that loads the table through this parser before
// the CronJob is applied. If that step is ever removed, this comment is the
// record of what it was for.
