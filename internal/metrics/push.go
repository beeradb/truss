package metrics

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// timeout bounds one push. Same reasoning and same number as
// deadman.Ping's: a monitoring endpoint that accepts a connection and then
// never answers must not hang a pass that holds a state lock. Telemetry is
// the last thing a pass does and the least important thing it does.
const timeout = 5 * time.Second

// contentType is the exposition format Render writes. The Pushgateway
// accepts a body with no Content-Type at all, and then guesses; saying it
// removes the guess.
const contentType = "text/plain; version=0.0.4"

// Push sends one pass's whole exposition to a Pushgateway.
//
// baseURL is the gateway's root ("http://pushgateway.monitoring.svc:9091").
// group is the grouping key, job first: it becomes the URL path the gateway
// derives its labels from, and it is what decides which previously pushed
// metrics this push REPLACES.
//
// ⚠️ PUT, NOT POST, AND THE DIFFERENCE IS A WHOLE CLASS OF STALE ALERT. POST
// replaces only the metric families named in this body and leaves every other
// family in the group untouched; PUT replaces the group entirely. A pass that
// stops emitting a family -- because the root it named is gone, because the
// credential it named was renewed -- must not leave the last value it ever
// had sitting in the gateway forever, answering queries as though it were
// current. Truss pushes its complete state every pass, so replacing the group
// is both correct and the only way a metric can ever go away.
//
// ⚠️ THE RETURNED ERROR NEVER CARRIES url, for exactly the reason
// deadman.Ping's does not: a push endpoint can carry credentials in its
// userinfo ("https://user:pass@..."), and net/http embeds the request URL
// verbatim in every error it returns. Push builds its own description
// instead, so a caller may log what it gets with no redaction step of its own
// to remember.
func Push(ctx context.Context, baseURL string, group []Label, body string) error {
	url, err := pushURL(baseURL, group)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, strings.NewReader(body))
	if err != nil {
		return errors.New("could not build the metrics push request")
	}
	req.Header.Set("Content-Type", contentType)

	// An explicit client with its own Timeout, matching internal/forge and
	// internal/deadman, and required by cmd/truss's own
	// TestNoHTTPClientIsUnbounded: the context deadline above already bounds
	// this call, but a client with no Timeout of its own is the shape that
	// check refuses regardless of what a caller happens to do with it.
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("could not reach the metrics gateway")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("the metrics gateway returned status %d", resp.StatusCode)
	}
	return nil
}

// pushURL builds the gateway's grouping-key path: /metrics/job/<job>/<name>/<value>...
//
// ⚠️ IT REFUSES A VALUE IT WOULD HAVE TO ESCAPE INSTEAD OF ESCAPING ONE. The
// Pushgateway has a base64 convention for grouping-key values containing a
// slash or an empty value ("name@base64/<value>"), and supporting it would
// make this function the place where a subtly wrong label silently splits
// into two path segments and pushes into a group nobody is querying. Every
// grouping key truss uses is a constant of its own choosing, so a value that
// needs escaping is a programming error and says so.
func pushURL(baseURL string, group []Label) (string, error) {
	if baseURL == "" {
		return "", errors.New("metrics: no gateway URL")
	}
	if len(group) == 0 {
		return "", errors.New("metrics: a push needs a grouping key")
	}
	if group[0].Name != "job" {
		return "", errors.New("metrics: the first grouping-key label must be job")
	}

	var b strings.Builder
	b.WriteString(strings.TrimSuffix(baseURL, "/"))
	b.WriteString("/metrics")
	for _, l := range group {
		if !validLabelName(l.Name) {
			return "", fmt.Errorf("metrics: %q is not a valid grouping-key label name", l.Name)
		}
		if l.Value == "" || strings.ContainsAny(l.Value, "/?#") {
			return "", fmt.Errorf("metrics: grouping-key label %s has a value this path cannot carry unescaped", l.Name)
		}
		b.WriteString("/")
		b.WriteString(l.Name)
		b.WriteString("/")
		b.WriteString(l.Value)
	}
	return b.String(), nil
}
