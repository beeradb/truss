package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// buildFakebinAsTofu compiles this package to <dir>/tofu, the same way
// internal/parity/harness.go builds it -- gatedApply only takes effect when
// the binary is invoked AS "tofu" (main.run dispatches on
// filepath.Base(os.Args[0])), so the test needs the real binary under that
// name rather than the test binary re-executing itself.
func buildFakebinAsTofu(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "tofu")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/beeradb/truss/internal/parity/fakebin").CombinedOutput()
	if err != nil {
		t.Fatalf("building fakebin as tofu: %v\n%s", err, out)
	}
	return bin
}

func writeFixtures(t *testing.T, dir string, f fixtures) {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshalling fixtures: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixtures.json"), data, 0o600); err != nil {
		t.Fatalf("writing fixtures.json: %v", err)
	}
}

// TestGatedApplyBlocksUntilReleased is gatedApply's basic contract: it must
// not exit merely because it was asked to run -- a caller synchronising on
// ".started" needs the process to still be there to signal.
func TestGatedApplyBlocksUntilReleased(t *testing.T) {
	dir := t.TempDir()
	bin := buildFakebinAsTofu(t, dir)
	gate := filepath.Join(dir, "gate")
	record := filepath.Join(dir, "record.json")
	writeFixtures(t, dir, fixtures{ApplyGate: gate, RecordPath: record})

	cmd := exec.Command(bin, "apply")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting fakebin: %v", err)
	}
	done := processDone(cmd)

	waitForFile(t, gate+".started", 2*time.Second)

	// Give it a moment to prove it is NOT exiting on its own.
	select {
	case err := <-done:
		t.Fatalf("fakebin exited before the gate was released (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}

	if err := os.WriteFile(gate+".release", nil, 0o600); err != nil {
		t.Fatalf("writing release marker: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fakebin exited non-zero after a plain release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("fakebin did not exit within 2s of release")
	}

	rec := readRecord(t, record)
	if rec.SignalCaught != "" {
		t.Errorf("SignalCaught = %q, want empty: nothing was ever sent", rec.SignalCaught)
	}
	if rec.Pid == 0 {
		t.Errorf("record carries no pid")
	}
}

// TestGatedApplyRecordsASignalSentToItsGroup is the property loop mode's
// graceful stop depends on: a SIGINT to the process group reaches this
// child while it is blocked, and it records having caught it rather than
// dying silently.
func TestGatedApplyRecordsASignalSentToItsGroup(t *testing.T) {
	dir := t.TempDir()
	bin := buildFakebinAsTofu(t, dir)
	gate := filepath.Join(dir, "gate")
	record := filepath.Join(dir, "record.json")
	writeFixtures(t, dir, fixtures{ApplyGate: gate, RecordPath: record})

	cmd := exec.Command(bin, "apply")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting fakebin: %v", err)
	}
	done := processDone(cmd)

	waitForFile(t, gate+".started", 2*time.Second)

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatalf("signalling fakebin's group: %v", err)
	}
	if err := os.WriteFile(gate+".release", nil, 0o600); err != nil {
		t.Fatalf("writing release marker: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("fakebin exited 0 after being interrupted, want non-zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("fakebin did not exit within 2s of release")
	}

	rec := readRecord(t, record)
	if rec.SignalCaught != "interrupt" {
		t.Errorf("SignalCaught = %q, want %q", rec.SignalCaught, "interrupt")
	}
	if rec.Pgid != rec.Pid {
		t.Errorf("Pgid = %d, Pid = %d, want equal: fakebin should be its own group leader under Setpgid", rec.Pgid, rec.Pid)
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %s", path, timeout)
}

// processDone runs cmd.Wait in a goroutine and delivers its result exactly
// once, so every caller in this file waits on the same channel rather than
// calling cmd.Wait itself -- os/exec documents that calling Wait twice on
// one Cmd is an error.
func processDone(cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}

func readRecord(t *testing.T, path string) applyRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading record %s: %v", path, err)
	}
	var rec applyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("unmarshalling record: %v", err)
	}
	return rec
}
