// Package qemunbdd implements ebsprovider.EBSProvider over qcow2 files and
// qemu-nbd. It shares no code with viperblock: every operation shells out to
// qemu-img, qemu-io or qemu-nbd, and a volume's only state is its file.
package qemunbdd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// runner executes a host command and returns its combined output. Every
// qemu-img, qemu-io and qemu-nbd invocation goes through this seam so unit
// tests can assert argv without a qemu installation.
type runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// execRunner is the production runner, shelling out via os/exec.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}
