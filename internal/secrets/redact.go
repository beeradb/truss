package secrets

import "strings"

// redact replaces every occurrence of each non-empty value in s with "***".
// Used everywhere an error message is built from a response body or a
// transport error that might, in principle, echo back the client token or
// the JWT this package logged in with -- neither may ever appear in
// anything this package returns (§4.7 "Refuses to": "log or return any
// credential value, the Vault token, or the JWT").
func redact(s string, values ...string) string {
	for _, v := range values {
		if v == "" {
			continue
		}
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
