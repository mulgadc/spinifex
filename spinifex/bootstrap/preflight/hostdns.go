package preflight

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const resolvConf = "/etc/resolv.conf"

// resolvedStubs are the files that send every lookup through systemd-resolved:
// its runtime stub and the static copy it ships.
var resolvedStubs = []string{
	"/run/systemd/resolve/stub-resolv.conf",
	"/usr/lib/systemd/resolv.conf",
	"/lib/systemd/resolv.conf",
}

// checkHostDNS warns when systemd-resolved runs but /etc/resolv.conf bypasses
// it. The Spinifex zones are routed to northstar inside resolved, so lookups
// that skip it never see them.
func checkHostDNS(root string) Result {
	const unit = "systemd-resolved.service"
	if !unitActive(unit) {
		return Result{Path: resolvConf, Kind: "host-dns", Status: OK}
	}
	target, err := os.Readlink(filepath.Join(root, resolvConf))
	if err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(resolvConf), target)
		}
		if slices.Contains(resolvedStubs, filepath.Clean(target)) {
			return Result{Path: resolvConf, Kind: "host-dns", Status: OK}
		}
	}
	what := "is not a link"
	if err == nil {
		what = fmt.Sprintf("links to %s", target)
	}
	return Result{Path: resolvConf, Kind: "host-dns", Status: Warning,
		Detail: fmt.Sprintf("%s is active but %s %s to its stub, so Spinifex names may not resolve on this host; run 'ln -sf %s %s'",
			unit, resolvConf, what, resolvedStubs[0], resolvConf)}
}
