package ledger

import "strings"

// Layout names the keys the applier reads and writes, verbatim from
// config.Config -- AppliedPrefix, FailedPrefix and PlanDigestPrefix are
// prefixes joined with a sha or a root; HeadKey and HeartbeatKey are
// complete keys on their own (they hold exactly one object each, so there
// is nothing to join them with).
type Layout struct {
	AppliedPrefix    string
	FailedPrefix     string
	HeadKey          string
	HeartbeatKey     string
	PlanDigestPrefix string
}

// AppliedKey is where a commit's applied (or noop) record lives:
// "<AppliedPrefix>/<sha>".
func (l Layout) AppliedKey(sha string) string {
	return l.AppliedPrefix + "/" + sha
}

// FailedKey is where a commit's failure record lives:
// "<FailedPrefix>/<sha>".
func (l Layout) FailedKey(sha string) string {
	return l.FailedPrefix + "/" + sha
}

// DigestKey is where the plan digest CI approved for one root, at one PR
// head sha, lives: "<PlanDigestPrefix>/<headSHA>/<slug>.digest", where slug
// replaces EVERY "/" in root with "-". This must agree byte for byte with
// the key CI writes the digest under (§4.2), and EVERY occurrence is
// replaced, not just the first: a root like "a/b/c" has to reach one key,
// and a slug that stopped at the first slash would put two different roots
// at the same one.
func (l Layout) DigestKey(headSHA, root string) string {
	slug := strings.ReplaceAll(root, "/", "-")
	return l.PlanDigestPrefix + "/" + headSHA + "/" + slug + ".digest"
}
