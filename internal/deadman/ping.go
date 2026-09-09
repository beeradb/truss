// Package deadman sends the liveness ping an external dead-man's-switch
// monitor (Healthchecks.io, Cronitor, Dead Man's Snitch, a Prometheus
// absent() rule watching a pushed metric, ...) expects on every completed
// pass. It answers exactly one question -- is the applier still running --
// and deliberately carries no outcome: the chat message is where an outcome
// belongs. See config.Config.HeartbeatPingURL and runApplyPass's tail.
package deadman

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// timeout bounds one ping. A hung monitor must not hang a pass that holds a
// state lock -- the same reasoning applyOneRoot's ErrLockBusy handling
// already carries for every other call this pass makes.
const timeout = 5 * time.Second

// Ping sends a liveness GET to url and reports whether the monitor
// acknowledged it (any 2xx). Every error is for a caller's non-fatal log
// line only, matching notify.Telegram.Send's own contract.
//
// ⚠️ THE RETURNED ERROR NEVER CARRIES url. A dead-man's-switch URL is a
// bearer secret -- its path segment (a UUID, for Healthchecks.io) is the
// only credential a monitor checks -- and net/http's own errors embed the
// request URL verbatim (e.g. `Get "https://hc-ping.com/<uuid>": dial tcp
// ...`). Ping never returns that text; it builds its own description instead,
// so a caller can log the error's Error() outright with no redaction step of
// its own to get right or forget.
func Ping(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return errors.New("could not build the ping request")
	}

	// An explicit client with its own Timeout, matching internal/forge's own
	// convention (and the repo-wide rule enforced by
	// cmd/truss/git_token_test.go's TestNoHTTPClientIsUnbounded): a monitor
	// that accepts a connection and never answers must not hang a pass that
	// holds a state lock. The context deadline above already bounds this,
	// but a client with no Timeout field of its own is the shape that check
	// flags regardless of what callers happen to do with it.
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("could not reach the monitor")
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("monitor returned status %d", resp.StatusCode)
	}
	return nil
}
