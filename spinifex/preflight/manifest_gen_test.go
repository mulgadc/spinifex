package preflight

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot walks up from this test file's directory until scripts/setup.sh
// is found, so the drift guard locates the checkout regardless of where
// `go test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed to resolve this test file's path")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "setup.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root: no scripts/setup.sh found walking up from test file")
		}
		dir = parent
	}
}

// extractHeredoc mirrors scripts/gen-asset-manifest.sh's extraction: the
// body of the first quoted 'HELPER' heredoc starting after the first line
// containing anchor. Both provisioning scripts hold more than one such heredoc.
func extractHeredoc(file, anchor string) ([]byte, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")

	anchorLine := -1
	for i, line := range lines {
		if strings.Contains(line, anchor) {
			anchorLine = i
			break
		}
	}
	if anchorLine < 0 {
		return nil, fmt.Errorf("anchor %q not found in %s", anchor, file)
	}

	startLine := -1
	for i := anchorLine + 1; i < len(lines); i++ {
		if strings.Contains(lines[i], "<<") && strings.Contains(lines[i], "HELPER") {
			startLine = i
			break
		}
	}
	if startLine < 0 {
		return nil, fmt.Errorf("no HELPER heredoc start found after anchor %q in %s", anchor, file)
	}

	var body []string
	for i := startLine + 1; i < len(lines); i++ {
		if lines[i] == "HELPER" {
			return []byte(strings.Join(body, "\n") + "\n"), nil
		}
		body = append(body, lines[i])
	}
	return nil, fmt.Errorf("no HELPER heredoc terminator found after anchor %q in %s", anchor, file)
}

// TestManifestMatchesProvisioningScripts re-extracts each managed heredoc
// and asserts the committed canonicalHashes still matches, turning an
// un-regenerated heredoc edit into a CI failure instead of silent drift.
func TestManifestMatchesProvisioningScripts(t *testing.T) {
	root := repoRoot(t)

	cases := []struct {
		path   string
		file   string
		anchor string
	}{
		{
			path:   "/usr/local/lib/spinifex/spinifex-set-endpoint-sysctl",
			file:   filepath.Join(root, "scripts", "setup.sh"),
			anchor: "install_endpoint_sysctl_helper()",
		},
		{
			path:   "/usr/local/lib/spinifex/ovs-socket-perms.sh",
			file:   filepath.Join(root, "scripts", "setup-ovn.sh"),
			anchor: "Step 8: Grant the spinifex service users access to OVS/OVN",
		},
	}

	for _, c := range cases {
		body, err := extractHeredoc(c.file, c.anchor)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}

		sum := sha256.Sum256(body)
		got := hex.EncodeToString(sum[:])

		want, ok := canonicalHashes[c.path]
		if !ok {
			t.Fatalf("%s: no entry in canonicalHashes; run scripts/gen-asset-manifest.sh", c.path)
		}
		if got != want {
			t.Errorf("%s: heredoc in %s hashes to %s, manifest has %s — run scripts/gen-asset-manifest.sh",
				c.path, c.file, got, want)
		}
	}
}
