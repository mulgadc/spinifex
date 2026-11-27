package viperblock

//test:in-package — drives the unexported shutdownVolumes and reuses the
// in-package doubles captureLogs and createTestVBWithState.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSS puts an ss first on PATH that logs each run to the returned file and
// then hangs, standing in for a socket listing that never returns.
func fakeSS(t *testing.T) (invocations string) {
	t.Helper()
	dir := t.TempDir()
	invocations = filepath.Join(dir, "ss.calls")
	script := "#!/bin/sh\necho run >> " + invocations + "\nexec sleep 60\n"
	if err := os.WriteFile(filepath.Join(dir, "ss"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ss: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return invocations
}

func startSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleeper: %v", err)
	}
	// Reap as soon as it exits, or a killed sleeper lingers as a zombie that
	// still reads as alive and the reap waits out its full grace period.
	reaped := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(reaped) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-reaped })
	return cmd.Process.Pid
}

// One shared deadline bounds the whole idle check. The first ss hangs and
// uses all of it, so no later volume may start a check, and no nbdkit that
// could not be checked may be killed.
func TestShutdownIdleCheckSharesOneDeadline(t *testing.T) {
	calls := fakeSS(t)
	cfg := &Config{}
	pids := make([]int, 3)
	for i := range pids {
		pids[i] = startSleeper(t)
		cfg.MountedVolumes = append(cfg.MountedVolumes, MountedVolume{
			Name: "vol-bound-" + string(rune('a'+i)), PID: pids[i],
			Socket: filepath.Join(t.TempDir(), "nbd.sock"),
		})
	}

	done := make(chan struct{})
	go func() { cfg.Shutdown(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown still waiting on a hung ss after 10s")
	}

	raw, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read ss invocation log: %v", err)
	}
	if n := strings.Count(string(raw), "run"); n != 1 {
		t.Errorf("ss ran %d times, want 1: checks after the shared deadline must not start", n)
	}
	for i, pid := range pids {
		if !alive(pid) {
			t.Errorf("nbdkit %d killed although its idle check never completed", i)
		}
	}
}

// Each volume's check runs after that volume's detach, so a guest that
// connects while an earlier volume detaches is still seen. The check here
// reports a connection to the second volume only once the first has detached.
func TestShutdownSeesAConnectionThatArrivesDuringAnEarlierDetach(t *testing.T) {
	logs := captureLogs(t)
	names := []string{"vol-latefirst", "vol-latesecond"}
	volumes := make([]MountedVolume, len(names))
	for i, name := range names {
		volumes[i] = MountedVolume{Name: name, VB: createTestVBWithState(t, name), PID: startSleeper(t)}
	}

	var probedBeforeDetach []string
	inUse := func(v MountedVolume) bool {
		if volumeCloseCount(logs.String(), v.Name) != 1 {
			probedBeforeDetach = append(probedBeforeDetach, v.Name)
		}
		return v.Name == names[1] && volumeCloseCount(logs.String(), names[0]) == 1
	}
	shutdownVolumes(volumes, inUse)

	if len(probedBeforeDetach) > 0 {
		t.Errorf("idle check ran before its own volume detached: %v", probedBeforeDetach)
	}
	if !alive(volumes[1].PID) {
		t.Error("nbdkit killed for a guest that connected during an earlier detach")
	}
}
