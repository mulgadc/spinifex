package main

import (
	"os/exec"
	"strings"
)

// commandRunner abstracts exec.Command so discovery is unit-testable without
// shelling out. Production uses execCommandRunner; tests inject a fake.
type commandRunner func(name string, args ...string) ([]byte, error)

// execCommandRunner runs a real host command and returns its stdout.
func execCommandRunner(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// discoverNvidiaGPUs enumerates local NVIDIA GPU device UUIDs via nvidia-smi.
// A missing binary or any run failure (no GPU host) yields an empty list, not
// an error — the agent still registers with zero GPU capacity.
func discoverNvidiaGPUs(run commandRunner) []string {
	out, err := run("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader")
	if err != nil {
		return nil
	}
	return parseNvidiaSMIUUIDs(string(out))
}

// parseNvidiaSMIUUIDs splits nvidia-smi's newline-delimited UUID output,
// dropping blank lines. Exported as a pure function so fixture strings can be
// tested without exec.
func parseNvidiaSMIUUIDs(out string) []string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	uuids := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		uuids = append(uuids, line)
	}
	return uuids
}
