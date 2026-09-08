package secrets

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// writeField writes root/item/field = value, creating the item directory.
func writeField(t *testing.T, root, item, field, value string) {
	t.Helper()
	dir := filepath.Join(root, item)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, field), []byte(value), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestFieldRefusesAMissingMount(t *testing.T) {
	d := Dir{Root: t.TempDir()}
	_, err := d.Field("nyt-cookie", "credential")
	if err == nil {
		t.Fatal("Field on a missing mount = nil error")
	}
	if !strings.Contains(err.Error(), "is not mounted") {
		t.Errorf("error %q does not say the mount is missing", err.Error())
	}
}

func TestAnEmptyFieldIsAsFatalAsAMissingOne(t *testing.T) {
	root := t.TempDir()
	writeField(t, root, "nyt-cookie", "credential", "")
	d := Dir{Root: root}

	_, err := d.Field("nyt-cookie", "credential")
	if err == nil {
		t.Fatal("Field on an empty file = nil error")
	}
	if !strings.Contains(err.Error(), "is empty") {
		t.Errorf("error %q does not say the field is empty", err.Error())
	}
}

func TestFieldSaysContinueNotStart(t *testing.T) {
	d := Dir{Root: t.TempDir()}
	_, err := d.Field("nyt-cookie", "credential")
	if err == nil {
		t.Fatal("Field on a missing mount = nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "refusing to continue") {
		t.Errorf("error %q does not read 'refusing to continue'", msg)
	}
	if strings.Contains(msg, "refusing to start") {
		t.Errorf("error %q reads 'refusing to start' -- Field is also called mid-run and must not misdate the incident", msg)
	}
}

// TestFieldNeverFallsBackToAnyRemoteCall pins two things: structurally,
// that Dir carries nothing a fallback could be wired to (a field beyond
// Root would be the first sign one had been added), and behaviourally,
// that a missing mount fails as fast as a pure filesystem check should --
// nothing here should ever be waiting on a dial.
func TestFieldNeverFallsBackToAnyRemoteCall(t *testing.T) {
	typ := reflect.TypeOf(Dir{})
	if typ.NumField() != 1 || typ.Field(0).Name != "Root" || typ.Field(0).Type.Kind() != reflect.String {
		t.Fatalf("Dir's shape changed to %v -- if that added anything network-shaped, Field must still never use it", typ)
	}

	d := Dir{Root: t.TempDir()}
	start := time.Now()
	if _, err := d.Field("nyt-cookie", "credential"); err == nil {
		t.Fatal("Field on a missing mount = nil error")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Field took %v to fail on a missing mount -- too long for a filesystem-only check, suggests a network attempt", elapsed)
	}
}

func TestFieldIfPresentDistinguishesAbsentFromUnreadable(t *testing.T) {
	t.Run("missing file is absent, not an error", func(t *testing.T) {
		d := Dir{Root: t.TempDir()}
		v, ok, err := d.FieldIfPresent("cf-infra-admin", "credential")
		if err != nil {
			t.Fatalf("FieldIfPresent on a missing file = %v, want no error", err)
		}
		if ok {
			t.Errorf("FieldIfPresent on a missing file = ok=true, v=%q, want ok=false", v)
		}
	})

	t.Run("a path that exists but is not a plain file is an error", func(t *testing.T) {
		root := t.TempDir()
		// A directory sitting where a field file should be: present on
		// disk, but not readable as a field. Portable across whether the
		// test runs as root, unlike a permission-denied fixture.
		dir := filepath.Join(root, "cf-infra-admin", "credential")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		d := Dir{Root: root}
		_, ok, err := d.FieldIfPresent("cf-infra-admin", "credential")
		if err == nil {
			t.Fatal("FieldIfPresent on a directory in place of the field = nil error, want an error")
		}
		if ok {
			t.Error("FieldIfPresent on an unreadable path = ok=true, want false")
		}
	})
}

func TestAnEmptyOptionalFieldReadsAsAbsent(t *testing.T) {
	root := t.TempDir()
	writeField(t, root, "cf-infra-admin", "credential", "")
	d := Dir{Root: root}

	v, ok, err := d.FieldIfPresent("cf-infra-admin", "credential")
	if err != nil {
		t.Fatalf("FieldIfPresent on an empty file = %v, want no error", err)
	}
	if ok {
		t.Errorf("FieldIfPresent on an empty file = ok=true, v=%q, want ok=false (reads as absent)", v)
	}
}

// TestNoValueAppearsInAnError guards against an error message built from
// more than the one field it is about -- e.g. by reading a whole item
// directory rather than the one field that was asked for.
func TestNoValueAppearsInAnError(t *testing.T) {
	root := t.TempDir()
	secretValue := strings.Join([]string{"nyt", "cookie", "value", "for", "this", "test", "only"}, "-")
	writeField(t, root, "nyt-cookie", "credential", secretValue)
	d := Dir{Root: root}

	// A sibling field in the SAME item directory is missing; Field must
	// refuse without ever mentioning the value that lives beside it.
	_, err := d.Field("nyt-cookie", "other-field")
	if err == nil {
		t.Fatal("Field on a missing sibling field = nil error")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("error %q leaked a value it never read", err.Error())
	}
}
