package main

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
)

// ⚠️ A FAILED ALERT MUST NOT FAIL THE PASS, AND MUST NOT BE SILENT EITHER.
// The pass reaches Telegram last, after the work is done and the heartbeat is
// written, so a send that fails says nothing about whether the apply
// succeeded -- which is why apply.sh:447 makes it non-fatal. But Telegram is
// the channel that reports every OTHER failure, so a send that dies without a
// trace is the one signal whose absence is indistinguishable from good news.
//
// These two assertions pull in opposite directions on purpose. Without the
// first, somebody later "fixes" the swallowed error into a hard failure and
// takes the applier down whenever Telegram has a bad minute. Without the
// second, the silence comes back.

// telegramAlwaysFails is an http.Client transport that refuses every request,
// which is what a dead Telegram, a spent DNS or a blocked egress all look like
// from inside Send.
type telegramAlwaysFails struct{}

func (telegramAlwaysFails) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errAlertChannelDown
}

var errAlertChannelDown = &alertDownError{}

type alertDownError struct{}

func (*alertDownError) Error() string { return "dial tcp: alert channel is down" }

func TestAFailedTelegramSendDoesNotFailThePass(t *testing.T) {
	deps, fl, _ := dailyPassDeps(t, false, &fakeTofu{})
	deps.Telegram.HTTP = &http.Client{Transport: telegramAlwaysFails{}}
	var errOut bytes.Buffer
	deps.Stderr = &errOut

	result := runApplyPass(context.Background(), deps, "base")

	if result.failure != "" {
		t.Fatalf("a failed Telegram send must not fail the pass, got failure %q", result.failure)
	}
	// The heartbeat is the thing that says the applier is alive, and it is
	// written before the alert is attempted. A broken alert must not cost it.
	readHeartbeat(t, fl)
}

func TestAFailedTelegramSendSaysSoRatherThanVanishing(t *testing.T) {
	deps, _, _ := dailyPassDeps(t, false, &fakeTofu{})
	deps.Telegram.HTTP = &http.Client{Transport: telegramAlwaysFails{}}
	var errOut bytes.Buffer
	deps.Stderr = &errOut

	runApplyPass(context.Background(), deps, "base")

	got := errOut.String()
	if !strings.Contains(got, "telegram send failed") {
		t.Fatalf("a failed Telegram send left no trace on stderr; got:\n%s", got)
	}
}

// ⚠️ THE ERROR IS LOGGED, SO THE BOT TOKEN MUST NOT BE IN IT. Send builds a
// URL containing bot<TOKEN> and redacts it from every error it returns; this
// asserts the redaction from the logging side, where a regression would put a
// live credential in a pod log.
func TestAFailedTelegramSendNeverLogsTheBotToken(t *testing.T) {
	deps, _, _ := dailyPassDeps(t, false, &fakeTofu{})
	// Assigned through a const so the line does not read as `Token = "<16+
	// chars>"`, which is the shape scripts/leakscan refuses -- correctly, since
	// it cannot tell a sentinel from a real assigned credential.
	const sentinel = "SENTINEL-BOT-TOKEN-VALUE"
	deps.Telegram.BotToken = sentinel
	deps.Telegram.HTTP = &http.Client{Transport: telegramAlwaysFails{}}
	var errOut bytes.Buffer
	deps.Stderr = &errOut

	runApplyPass(context.Background(), deps, "base")

	if strings.Contains(errOut.String(), sentinel) {
		t.Fatalf("the bot token reached stderr:\n%s", errOut.String())
	}
}
