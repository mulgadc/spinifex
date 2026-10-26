package preflight

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ufwConfig records whether ufw is enabled. ufw.service is a oneshot that stays
// active after `ufw disable`, so the unit state alone does not say ufw is on.
const ufwConfig = "/etc/ufw/ufw.conf"

// unitActive is a variable so tests never query a real systemd.
var unitActive = func(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

// checkHostFirewalls fails while firewalld or ufw is active. A forwarded packet
// must then be accepted by their ruleset as well as Spinifex's, so routed NAT,
// EIPs and IMDS break with nothing logged.
func checkHostFirewalls(root string) []Result {
	return []Result{checkFirewalld(), checkUFW(root)}
}

func checkFirewalld() Result {
	const unit = "firewalld.service"
	if unitActive(unit) {
		return Result{Path: unit, Kind: "host-firewall", Status: Failed,
			Detail: "firewalld is active beside the Spinifex nftables policy; run 'systemctl mask --now firewalld'"}
	}
	return Result{Path: unit, Kind: "host-firewall", Status: OK}
}

func checkUFW(root string) Result {
	const unit = "ufw.service"
	if unitActive(unit) && ufwEnabled(root) {
		return Result{Path: unit, Kind: "host-firewall", Status: Failed,
			Detail: "ufw is enabled beside the Spinifex nftables policy; run 'ufw disable'"}
	}
	return Result{Path: unit, Kind: "host-firewall", Status: OK}
}

// ufwEnabled reads ENABLED= from ufw.conf, as ufw-init does. A missing file
// means ufw is not installed.
func ufwEnabled(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, ufwConfig))
	if err != nil {
		return false
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if ok && key == "ENABLED" {
			value = strings.Trim(value, `"'`)
			return value == "yes" || value == "YES" // the two spellings ufw-init accepts
		}
	}
	return false
}
