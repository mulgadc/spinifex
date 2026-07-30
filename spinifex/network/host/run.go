package host

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// execRunner escalates only the commands that need it — see utils.NeedsPrivilege.
// The OVS/OVN socket clients reach their daemons over the group-owned control
// sockets, and ip/iptables/arping run under the unit's ambient capabilities.
type execRunner struct{}

var _ Runner = execRunner{}

// NewExecRunner returns the default Runner.
func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	var cmd *exec.Cmd
	if utils.NeedsPrivilege(name, args...) {
		cmd = exec.CommandContext(ctx, "sudo", append([]string{name}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, name, args...)
	}
	// Capture stdout and stderr separately so stderr is not folded into parsed
	// output: under a restrictive CapabilityBoundingSet (no CAP_AUDIT_WRITE) sudo
	// prints "unable to send audit message" to stderr while still succeeding.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// On failure hand back stdout+stderr so callers matching diagnostics on the
		// error path (e.g. "File exists" idempotency) still see them.
		out := append(stdout.Bytes(), stderr.Bytes()...)
		return out, fmt.Errorf("%s %s: %s: %w", name, strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return stdout.Bytes(), nil
}

// kernelReader satisfies InterfaceReader via the live kernel.
type kernelReader struct{}

var _ InterfaceReader = kernelReader{}

// NewKernelReader returns the default InterfaceReader.
func NewKernelReader() InterfaceReader { return kernelReader{} }

func (kernelReader) BridgeCIDR(name string) (netip.Prefix, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("interface %q: %w", name, err)
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("addrs %q: %w", name, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		v4 := ipnet.IP.To4()
		if v4 == nil {
			continue
		}
		ones, _ := ipnet.Mask.Size()
		addr, _ := netip.AddrFromSlice(v4)
		return netip.PrefixFrom(addr, ones), nil
	}
	return netip.Prefix{}, fmt.Errorf("%w: %q", ErrNoUplinkAddr, name)
}

func (kernelReader) LinkMAC(name string) (net.HardwareAddr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("interface %q: %w", name, err)
	}
	if len(iface.HardwareAddr) == 0 {
		return nil, fmt.Errorf("interface %q: no hardware address", name)
	}
	return iface.HardwareAddr, nil
}

func (kernelReader) LinkMaster(name string) (string, error) {
	target, err := os.Readlink(filepath.Join("/sys/class/net", name, "master"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return filepath.Base(target), nil
}
