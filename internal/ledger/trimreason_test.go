package ledger

import "testing"

func TestTrimReasonCutsAt800BytesAndSaysSo(t *testing.T) {
	short := "state lock timeout"
	if got := TrimReason(short); got != short {
		t.Errorf("short reason changed: got %q, want %q", got, short)
	}

	exactly800 := repeatByte('a', 800)
	if got := TrimReason(exactly800); got != exactly800 {
		t.Errorf("an exactly-800-byte reason must not be marked truncated, got %d bytes", len(got))
	}

	over := repeatByte('a', 900)
	got := TrimReason(over)
	wantPrefix := repeatByte('a', 800)
	if len(got) <= 800 {
		t.Fatalf("truncated reason has no marker appended: %d bytes", len(got))
	}
	if got[:800] != wantPrefix {
		t.Errorf("truncated reason does not start with the first 800 bytes of the original")
	}
	if got != wantPrefix+trimReasonMarker {
		t.Errorf("TrimReason(900 bytes) = %q, want the 800-byte prefix plus the marker", got)
	}
}

// TestTrimReasonCountsBytesNotRunes: the budget is 800 BYTES, and counting
// characters instead cuts a multibyte reason that is under 800 characters
// but over 800 bytes with no marker at all. "é" is two bytes in UTF-8, so
// 500 of them is 500 runes and 1000 bytes: over the byte limit, under a
// rune-counted one.
func TestTrimReasonCountsBytesNotRunes(t *testing.T) {
	reason := repeatRune('é', 500) // 500 runes, 1000 bytes
	if len(reason) != 1000 {
		t.Fatalf("test fixture is wrong: len(reason) = %d, want 1000 bytes", len(reason))
	}

	got := TrimReason(reason)
	if len(got) == len(reason) {
		t.Fatalf("TrimReason did not truncate a 1000-byte, 500-rune reason: a rune-counting implementation would stop here")
	}
	wantBody := reason[:800]
	if got != wantBody+trimReasonMarker {
		t.Errorf("TrimReason cut at the wrong byte boundary: got %d bytes before the marker, want 800", len(got)-len(trimReasonMarker))
	}
}

// TestTrimReasonDropsNULBytes: NULs are stripped before the 800-byte cut,
// but the truncation MARKER is decided from the length of the ORIGINAL,
// un-stripped text. So a reason that is only over 800 bytes because of NUL
// padding still gets the marker appended, even though the stripped content
// alone is short. That is deliberate and TrimReason's own doc comment says
// why: the marker's claim is that what you are reading is not what the run
// produced, and once NULs have been taken out that is already true.
func TestTrimReasonDropsNULBytes(t *testing.T) {
	short := "boom"
	padded := short + string(make([]byte, 1000)) // 1000 NUL bytes appended

	got := TrimReason(padded)
	if containsNUL(got) {
		t.Errorf("TrimReason left a NUL byte in the result: %q", got)
	}
	want := short + trimReasonMarker
	if got != want {
		t.Errorf("TrimReason(short+1000 NULs) = %q, want %q (marker appended per the original length, even though stripped content is short)", got, want)
	}
}

func containsNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

func repeatByte(b byte, n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = b
	}
	return string(buf)
}

func repeatRune(r rune, n int) string {
	buf := make([]rune, n)
	for i := range buf {
		buf[i] = r
	}
	return string(buf)
}
