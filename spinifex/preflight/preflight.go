// Package preflight verifies a host's managed helper scripts and sudoers
// grants — which a binary-swap deploy does not ship — against what this
// build of spinifex expects, comparing content so a stale asset is caught.
package preflight

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

//go:generate ../../scripts/gen-asset-manifest.sh

// Status is the outcome of checking one managed asset against this build's manifest.
type Status int

const (
	// OK means the on-host asset matches what this build expects.
	OK Status = iota
	// Missing means the asset is absent from the host.
	Missing
	// Stale means the asset is present but its content does not match this build's manifest.
	Stale
	// Ungranted means the sudoers grant for a helper is absent or missing its NOPASSWD line.
	Ungranted
)

// String returns the human-readable name of the status, used in table output and log lines.
func (s Status) String() string {
	switch s {
	case OK:
		return "OK"
	case Missing:
		return "Missing"
	case Stale:
		return "Stale"
	case Ungranted:
		return "Ungranted"
	default:
		return "Unknown"
	}
}

// Result is the outcome of checking one managed asset against this build's manifest.
type Result struct {
	Path   string
	Kind   string // "helper" | "sudoers-grant"
	Status Status
	Detail string
}

// managedHelpers lists every helper script this build expects on the host,
// keyed by installed path. An explicit list, not a directory scan, so adding
// an asset here is a reviewed change and never accidental.
var managedHelpers = []string{
	utils.EndpointSysctlHelper,
	"/usr/local/lib/spinifex/ovs-socket-perms.sh",
}

// sudoersGrantFile is assembled with substitutions and multiple rules by
// setup.sh, so it is not hashable as a unit; the check below looks for the
// specific NOPASSWD line for the sysctl helper.
const sudoersGrantFile = "/etc/sudoers.d/spinifex-network"

// CheckHost verifies every managed asset on the real host against this build's manifest.
func CheckHost() []Result {
	return CheckHostAt("/")
}

// CheckHostAt verifies every managed asset with paths resolved under root.
// It is the test seam CheckHost uses with root "/".
func CheckHostAt(root string) []Result {
	results := make([]Result, 0, len(managedHelpers)+1)
	for _, path := range managedHelpers {
		results = append(results, checkHelper(root, path))
	}
	results = append(results, checkSudoersGrant(root))
	return results
}

// HasProblem reports whether any result is not OK.
func HasProblem(results []Result) bool {
	for _, r := range results {
		if r.Status != OK {
			return true
		}
	}
	return false
}

func checkHelper(root, path string) Result {
	want, ok := canonicalHashes[path]
	if !ok {
		// managedHelpers and canonicalHashes are both generated from the same
		// asset list; a miss here means the manifest needs regenerating.
		return Result{Path: path, Kind: "helper", Status: Missing, Detail: "no canonical hash for this build — run scripts/gen-asset-manifest.sh"}
	}

	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		return Result{Path: path, Kind: "helper", Status: Missing, Detail: err.Error()}
	}

	got := hashHex(data)
	if got != want {
		return Result{
			Path:   path,
			Kind:   "helper",
			Status: Stale,
			Detail: fmt.Sprintf("installed %s, expected %s", shortHash(got), shortHash(want)),
		}
	}
	return Result{Path: path, Kind: "helper", Status: OK}
}

func checkSudoersGrant(root string) Result {
	data, err := os.ReadFile(filepath.Join(root, sudoersGrantFile))
	if err != nil {
		return Result{Path: sudoersGrantFile, Kind: "sudoers-grant", Status: Ungranted, Detail: err.Error()}
	}

	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "NOPASSWD") && strings.Contains(trimmed, utils.EndpointSysctlHelper) {
			return Result{Path: sudoersGrantFile, Kind: "sudoers-grant", Status: OK}
		}
	}
	return Result{
		Path:   sudoersGrantFile,
		Kind:   "sudoers-grant",
		Status: Ungranted,
		Detail: "no NOPASSWD grant line for " + utils.EndpointSysctlHelper,
	}
}

func hashHex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// shortHash truncates a hex digest for compact Detail messages.
func shortHash(h string) string {
	const n = 12
	if len(h) > n {
		return h[:n]
	}
	return h
}
