package secrets

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Dir reads credentials rendered as files under Root by the vault-secrets
// init container: $SECRETS_DIR/<item>/<field>. This is "the seam,
// backend-agnostic on purpose" (docs/decisions/vault.md) and decision 4
// does not reach it -- only what fills the files changed when the backend
// did. Dir never dials a network: a missing or unreadable file is the only
// question it can ask, and it asks it against the filesystem alone.
type Dir struct{ Root string }

// Field reads one required credential field. A missing mount is fatal
// (apply.sh:136); an EMPTY field is equally fatal (apply.sh:141) -- an item
// whose credential mirror wrote a zero-byte file is not a value anyone can
// use, and treating it as present would hand the caller an empty string
// with no signal anything is wrong. The message says "refusing to
// continue" rather than "refusing to start": Field is also called mid-run,
// once read_apply_credentials has already decided the pass has work, and a
// message claiming a boot failure would misdate the incident.
func (d Dir) Field(item, field string) (string, error) {
	path := filepath.Join(d.Root, item, field)

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to continue: %s is not mounted -- is the credential mirror applied and syncing?", path)
	}

	b, _ := os.ReadFile(path)
	// Command substitution in the bash ($(cat "$f")) strips every trailing
	// newline; match that so a value differs from the bash's only in bytes
	// nobody meant to be part of it.
	v := strings.TrimRight(string(b), "\n")
	if v == "" {
		return "", fmt.Errorf("refusing to continue: %s is empty -- item %q is missing field %q", path, item, field)
	}
	return v, nil
}

// FieldIfPresent reads one optional credential field: cf-infra-admin alone
// (apply.sh:156-159), the one credential that may legitimately not exist
// yet because credentials/ mints it and that root may not have applied.
//
// A missing mount reads as absent (ok=false, err=nil) -- the caller's job
// is to tell "not yet minted" from "the deploy is wrong", which Field
// already refuses to do for everything else. An EMPTY file also reads as
// absent, on the same reasoning Field applies to a required field: the
// credential mirror never intentionally writes a zero-byte value, so an
// empty file is indistinguishable from nothing having been minted, and
// FieldIfPresent's whole purpose is to let the caller make that call
// itself rather than dying on the difference.
//
// A path that exists but cannot be read as a plain file -- permission
// denied, or something other than a regular file at that path -- is
// neither: it is reported as an error, because the mount plainly is there
// and something about it is wrong, which absence-and-carry-on must not
// paper over.
func (d Dir) FieldIfPresent(item, field string) (string, bool, error) {
	path := filepath.Join(d.Root, item, field)

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("checking %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("%s exists but is not a regular file", path)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("reading %s: %w", path, err)
	}
	v := strings.TrimRight(string(b), "\n")
	if v == "" {
		return "", false, nil
	}
	return v, true, nil
}
