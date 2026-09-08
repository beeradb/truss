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
// "<AppliedPrefix>/<sha>", matching ledger_put_applied (apply.sh:334).
func (l Layout) AppliedKey(sha string) string {
	return l.AppliedPrefix + "/" + sha
}

// FailedKey is where a commit's failure record lives:
// "<FailedPrefix>/<sha>", matching ledger_put_failed (apply.sh:356).
func (l Layout) FailedKey(sha string) string {
	return l.FailedPrefix + "/" + sha
}

// DigestKey is where the plan digest CI approved for one root, at one PR
// head sha, lives: "<PlanDigestPrefix>/<headSHA>/<slug>.digest", where slug
// replaces EVERY "/" in root with "-". This must agree with what
// publish-plan-digest writes (port-plan.md §4.2) and with apply.sh:637
// (`echo "$root" | tr / -`), which replaces every occurrence, not just the
// first.
func (l Layout) DigestKey(headSHA, root string) string {
	slug := strings.ReplaceAll(root, "/", "-")
	return l.PlanDigestPrefix + "/" + headSHA + "/" + slug + ".digest"
}
