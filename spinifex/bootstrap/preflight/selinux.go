package preflight

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	selinuxEnforce = "/sys/fs/selinux/enforce"
	selinuxConfig  = "/etc/selinux/config"
	// selinuxModuleDir is the policy store entry semodule -X 300 -i creates;
	// %s is the policy type from the SELINUXTYPE line.
	selinuxModuleDir = "/var/lib/selinux/%s/active/modules/300/spinifex"
)

// selinuxLabel is a path whose SELinux type the Spinifex module sets. A path
// that is optional is checked only where it exists.
type selinuxLabel struct {
	path     string
	wantType string
	optional bool
}

// selinuxLabels are the paths a service or swapon fails on when their type is
// wrong. systemd cannot execute a var_lib_t script under enforcing.
var selinuxLabels = []selinuxLabel{
	{path: "/var/lib/spinifex/wait-for-nats.sh", wantType: "bin_t"},
	{path: "/var/lib/spinifex/nats-js-watchdog.sh", wantType: "bin_t"},
	{path: "/swapfile", wantType: "swapfile_t", optional: true},
	{path: "/etc/ipsec.secrets", wantType: "ipsec_key_file_t", optional: true},
}

// fileLabel is a variable so tests never need a host that stores SELinux labels.
var fileLabel = func(path string) (string, error) {
	buf := make([]byte, 256)
	n, err := unix.Lgetxattr(path, "security.selinux", buf)
	if err != nil {
		return "", err
	}
	return string(bytes.TrimRight(buf[:n], "\x00")), nil
}

// checkSELinux reports the SELinux mode, the Spinifex module and the labels it
// sets. A host with no SELinux at all, as on Debian and Ubuntu, gets no result.
func checkSELinux(root string) []Result {
	mode, err := os.ReadFile(filepath.Join(root, selinuxEnforce))
	if errors.Is(err, fs.ErrNotExist) {
		if _, cerr := os.Stat(filepath.Join(root, selinuxConfig)); cerr != nil {
			return nil
		}
		return []Result{{Path: selinuxConfig, Kind: "selinux", Status: Warning,
			Detail: "SELinux is disabled, so it confines nothing on this host"}}
	}
	if err != nil {
		return []Result{{Path: selinuxEnforce, Kind: "selinux", Status: Failed,
			Detail: fmt.Sprintf("cannot read the SELinux mode: %v", err)}}
	}

	results := []Result{checkSELinuxMode(strings.TrimSpace(string(mode)))}
	results = append(results, checkSELinuxModule(root))
	for _, l := range selinuxLabels {
		if r, ok := checkSELinuxLabel(root, l); ok {
			results = append(results, r)
		}
	}
	return results
}

func checkSELinuxMode(mode string) Result {
	switch mode {
	case "1":
		return Result{Path: selinuxEnforce, Kind: "selinux", Status: OK, Detail: "enforcing"}
	case "0":
		return Result{Path: selinuxEnforce, Kind: "selinux", Status: Warning,
			Detail: "SELinux is permissive and enforces nothing; when every selinux check is OK, run 'setenforce 1' and set SELINUX=enforcing in " + selinuxConfig}
	default:
		return Result{Path: selinuxEnforce, Kind: "selinux", Status: Failed,
			Detail: fmt.Sprintf("unknown SELinux mode %q", mode)}
	}
}

func checkSELinuxModule(root string) Result {
	dir := fmt.Sprintf(selinuxModuleDir, selinuxPolicyType(root))
	_, err := os.Stat(filepath.Join(root, dir))
	switch {
	case err == nil:
		return Result{Path: dir, Kind: "selinux", Status: OK}
	case errors.Is(err, fs.ErrPermission):
		return Result{Path: dir, Kind: "selinux", Status: Warning,
			Detail: "cannot read the SELinux policy store; run 'sudo spx admin preflight' to check the spinifex module"}
	case errors.Is(err, fs.ErrNotExist):
		return Result{Path: dir, Kind: "selinux", Status: Failed,
			Detail: "the spinifex SELinux module is not loaded; re-run the Spinifex installer"}
	default:
		return Result{Path: dir, Kind: "selinux", Status: Failed, Detail: err.Error()}
	}
}

// selinuxPolicyType reads SELINUXTYPE from the config, as libselinux does,
// and falls back to the targeted policy.
func selinuxPolicyType(root string) string {
	f, err := os.Open(filepath.Join(root, selinuxConfig))
	if err != nil {
		return "targeted"
	}
	defer func() { _ = f.Close() }()
	policy := "targeted"
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "SELINUXTYPE="); ok {
			if v = strings.Trim(v, `"'`); v != "" {
				policy = v
			}
		}
	}
	return policy
}

func checkSELinuxLabel(root string, l selinuxLabel) (Result, bool) {
	label, err := fileLabel(filepath.Join(root, l.path))
	if errors.Is(err, fs.ErrNotExist) {
		if l.optional {
			return Result{}, false
		}
		return Result{Path: l.path, Kind: "selinux", Status: Missing, Detail: err.Error()}, true
	}
	if err != nil {
		return Result{Path: l.path, Kind: "selinux", Status: Failed,
			Detail: fmt.Sprintf("cannot read the SELinux label: %v", err)}, true
	}
	// A context is user:role:type:level, and the level can hold more colons.
	parts := strings.SplitN(label, ":", 4)
	if len(parts) >= 3 && parts[2] == l.wantType {
		return Result{Path: l.path, Kind: "selinux", Status: OK}, true
	}
	return Result{Path: l.path, Kind: "selinux", Status: Failed,
		Detail: fmt.Sprintf("labelled %q, want type %s; run 'restorecon -F %s'", label, l.wantType, l.path)}, true
}
