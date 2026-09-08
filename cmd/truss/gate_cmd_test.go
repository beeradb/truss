package main

import (
	"bytes"
	"context"
	"testing"
)

const testSHA = "commitsha1"

// TestGateCommitPrintsTheOneLineProtocol drives `gate commit <sha>` against
// a fake GitHub that answers with a fully-compliant commit (one merged PR,
// approved by the approver at the head sha, merged by a verified web-flow
// commit) and checks the exact one-line success protocol -- "OK\t<head
// sha>\t<pr number>\n" -- then breaks the scenario one way (no approval)
// and checks the exact refusal protocol -- "FAIL\t<reason>\n" -- with the
// matching exit codes from §4.9's contract (0 pass, 1 refuse).
func TestGateCommitPrintsTheOneLineProtocol(t *testing.T) {
	dir, write := testSecretsDir(t)
	writeGitHubAppSecret(t, write)

	t.Run("pass", func(t *testing.T) {
		srv := newFakeForge(t, testSHA, fakeForgeScenario{
			Approver:       "alice",
			PRMerged:       true,
			CommitVerified: true,
		})
		env := testFullEnv(dir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
		env = withForgeBaseURL(env, srv.URL)

		var stdout, stderr bytes.Buffer
		code := runEnv(context.Background(), []string{"gate", "commit", testSHA}, env, nil, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %s, stdout: %s)", code, stderr.String(), stdout.String())
		}
		want := "OK\t" + testSHA + "\t1\n"
		if stdout.String() != want {
			t.Fatalf("stdout = %q, want %q", stdout.String(), want)
		}
	})

	t.Run("refused", func(t *testing.T) {
		srv := newFakeForge(t, testSHA, fakeForgeScenario{
			Approver:       "alice",
			PRMerged:       true,
			CommitVerified: true,
			ReviewState:    "COMMENTED", // no APPROVED review exists
		})
		env := testFullEnv(dir.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
		env = withForgeBaseURL(env, srv.URL)

		var stdout, stderr bytes.Buffer
		code := runEnv(context.Background(), []string{"gate", "commit", testSHA}, env, nil, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("exit code = %d, want 1 (stdout: %s)", code, stdout.String())
		}
		if !bytesHasPrefix(stdout.Bytes(), "FAIL\t") {
			t.Fatalf("stdout = %q, want it to start with %q", stdout.String(), "FAIL\t")
		}
	})

	t.Run("could not ask", func(t *testing.T) {
		// No forge base URL override at all: the client's default
		// (https://api.github.com) is unreachable in this sandbox, so
		// every call fails at the transport -- "could not ask", exit 2,
		// message on stderr rather than the one-line protocol on stdout.
		dir3, write3 := testSecretsDir(t)
		writeGitHubAppSecret(t, write3)
		env := testFullEnv(dir3.Root, t.TempDir(), map[string]string{"APPROVER": "alice"})
		env = withForgeBaseURL(env, "http://forge.invalid") // refuses connections

		var stdout, stderr bytes.Buffer
		code := runEnv(context.Background(), []string{"gate", "commit", testSHA}, env, nil, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("exit code = %d, want 2 (stdout: %s, stderr: %s)", code, stdout.String(), stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("stdout = %q, want empty on a could-not-ask", stdout.String())
		}
	})
}

func bytesHasPrefix(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && string(b[:len(prefix)]) == prefix
}
