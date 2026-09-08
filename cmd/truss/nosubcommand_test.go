package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestNoSubcommandPrintsASecret is the guard the task calls out as the one
// that matters most: an empty or unrecognised subcommand must never echo
// anything reachable from the environment or the arguments it was given,
// including anything that looks like a credential.
//
// It plants real-shaped secrets in the environment this binary is allowed
// to read (a github-app private key, a ledger access key, a telegram bot
// token -- all through secrets.Dir, the one place a real deploy's
// credentials live) and ALSO passes a secret-shaped value as the
// subcommand argument itself, the way a person who fat-fingered
// `truss $TELEGRAM_BOT_TOKEN` would. Neither must appear anywhere in
// stdout or stderr.
//
// This test is deliberately breakable, not just passable: run.go's usage
// path prints a fixed constant and nothing else. While writing this test,
// changing that path to interpolate os.Args (a plausible "helpful" UX
// change -- "unknown subcommand %q") was confirmed to fail this test on
// its ARG-echoing assertion before being reverted; see the final report.
func TestNoSubcommandPrintsASecret(t *testing.T) {
	dir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)
	write(itemLedger, fieldLedgerEndpoint, "http://ledger.invalid")
	write(itemLedger, fieldLedgerAccessKey, "AKIALEDGERSECRETMARKERVALUE")
	write(itemLedger, fieldLedgerSecretKey, "LEDGER-SECRET-MARKER-9f3a1c7e2b")
	write(itemTelegram, fieldTelegramBotToken, "TELEGRAM-SECRET-MARKER-7b2e9f14")
	write(itemTelegram, fieldTelegramChatID, "-100200300")
	write(itemCFTokenMint, fieldCFCredential, "CF-MINT-SECRET-MARKER-a91d4e")

	markers := []string{
		"AKIALEDGERSECRETMARKERVALUE",
		"LEDGER-SECRET-MARKER-9f3a1c7e2b",
		"TELEGRAM-SECRET-MARKER-7b2e9f14",
		"CF-MINT-SECRET-MARKER-a91d4e",
	}

	env := testFullEnv(dir.Root, t.TempDir(), map[string]string{
		// A secret-shaped value in an env var this binary DOES read, to
		// prove that even a var config.Load consults never reaches the
		// usage path's output.
		"APPROVER": "APPROVER-SECRET-MARKER-c02f88",
	})
	markers = append(markers, "APPROVER-SECRET-MARKER-c02f88")

	markerArg := "ARG-SECRET-MARKER-5d81ee"

	for _, args := range [][]string{
		nil,                  // no subcommand at all
		{},                   // same, explicit
		{"bogus"},            // unrecognised subcommand
		{markerArg},          // the subcommand slot carries a secret-shaped value
		{"bogus", markerArg}, // a secret-shaped value elsewhere in argv
	} {
		var stdout, stderr bytes.Buffer
		code := runEnv(context.Background(), args, env, nil, &stdout, &stderr)

		if code != 2 {
			t.Errorf("args=%v: exit code = %d, want 2", args, code)
		}
		out := stdout.String()
		errOut := stderr.String()

		for _, m := range markers {
			if strings.Contains(out, m) {
				t.Errorf("args=%v: stdout contains a secret marker %q:\n%s", args, m, out)
			}
			if strings.Contains(errOut, m) {
				t.Errorf("args=%v: stderr contains a secret marker %q:\n%s", args, m, errOut)
			}
		}
		if strings.Contains(out, markerArg) || strings.Contains(errOut, markerArg) {
			t.Errorf("args=%v: the argument itself was echoed back (stdout: %q, stderr: %q)", args, out, errOut)
		}

		// Pin the exact behaviour, not just the absence of markers: usage
		// goes to stderr as a fixed constant, verbatim, with nothing
		// appended or interpolated, and stdout carries nothing at all. A
		// test that only grepped for markers would pass even if some
		// OTHER secret-shaped string were echoed by accident; this also
		// catches that.
		if out != "" {
			t.Errorf("args=%v: stdout = %q, want empty", args, out)
		}
		if errOut != usage {
			t.Errorf("args=%v: stderr = %q, want exactly the usage constant", args, errOut)
		}
	}
}
