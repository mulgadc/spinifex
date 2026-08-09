package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
)

// writeHelper writes the given content at the managed path under root,
// creating parent directories as needed.
func writeHelper(t *testing.T, root, path string, content []byte) {
	t.Helper()
	full := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

func TestCheckHostAt_Helper(t *testing.T) {
	path := utils.EndpointSysctlHelper
	want, ok := canonicalHashes[path]
	if !ok {
		t.Fatalf("no canonical hash for %s — is manifest_gen.go generated?", path)
	}

	t.Run("absent", func(t *testing.T) {
		root := t.TempDir()
		r := findResult(t, CheckHostAt(root), path)
		if r.Status != Missing {
			t.Errorf("status = %v, want Missing", r.Status)
		}
	})

	t.Run("wrong bytes", func(t *testing.T) {
		root := t.TempDir()
		writeHelper(t, root, path, []byte("not the canonical helper body\n"))
		r := findResult(t, CheckHostAt(root), path)
		if r.Status != Stale {
			t.Errorf("status = %v, want Stale", r.Status)
		}
		gotShort := shortHash(hashHex([]byte("not the canonical helper body\n")))
		wantShort := shortHash(want)
		if !strings.Contains(r.Detail, gotShort) || !strings.Contains(r.Detail, wantShort) {
			t.Errorf("Detail %q does not contain both short hashes (%s, %s)", r.Detail, gotShort, wantShort)
		}
	})

	t.Run("correct bytes", func(t *testing.T) {
		// Read the canonical body the same way the package does (via the
		// generated manifest + the real provisioning script), so this
		// exercises the real OK path rather than a hard-coded fixture.
		root := t.TempDir()
		body := canonicalHelperBody(t, path)
		writeHelper(t, root, path, body)
		r := findResult(t, CheckHostAt(root), path)
		if r.Status != OK {
			t.Errorf("status = %v, want OK (detail: %s)", r.Status, r.Detail)
		}
	})
}

func TestCheckHostAt_SudoersGrant(t *testing.T) {
	t.Run("file absent", func(t *testing.T) {
		root := t.TempDir()
		r := findResult(t, CheckHostAt(root), sudoersGrantFile)
		if r.Status != Ungranted {
			t.Errorf("status = %v, want Ungranted", r.Status)
		}
	})

	t.Run("file present without grant line", func(t *testing.T) {
		root := t.TempDir()
		writeHelper(t, root, sudoersGrantFile, []byte("spinifex-daemon ALL=(root) NOPASSWD: /sbin/ip, /usr/sbin/ip\n"))
		r := findResult(t, CheckHostAt(root), sudoersGrantFile)
		if r.Status != Ungranted {
			t.Errorf("status = %v, want Ungranted", r.Status)
		}
	})

	t.Run("file present with grant line", func(t *testing.T) {
		root := t.TempDir()
		writeHelper(t, root, sudoersGrantFile, []byte(
			"spinifex-daemon ALL=(root) NOPASSWD: /sbin/ip, /usr/sbin/ip\n"+
				"spinifex-daemon ALL=(root) NOPASSWD: "+utils.EndpointSysctlHelper+"\n"))
		r := findResult(t, CheckHostAt(root), sudoersGrantFile)
		if r.Status != OK {
			t.Errorf("status = %v, want OK (detail: %s)", r.Status, r.Detail)
		}
	})
}

func TestHasProblem(t *testing.T) {
	if HasProblem([]Result{{Status: OK}, {Status: OK}}) {
		t.Error("HasProblem = true for all-OK results, want false")
	}
	if !HasProblem([]Result{{Status: OK}, {Status: Stale}}) {
		t.Error("HasProblem = false with a Stale result present, want true")
	}
}

func TestStatusString(t *testing.T) {
	cases := map[Status]string{OK: "OK", Missing: "Missing", Stale: "Stale", Ungranted: "Ungranted"}
	for status, want := range cases {
		if got := status.String(); got != want {
			t.Errorf("Status(%d).String() = %q, want %q", status, got, want)
		}
	}
}

// findResult returns the Result for path, failing the test if absent.
func findResult(t *testing.T, results []Result, path string) Result {
	t.Helper()
	for _, r := range results {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no Result for path %s in %+v", path, results)
	return Result{}
}

// canonicalHelperBody re-extracts the canonical bytes for path from the
// provisioning scripts, so the OK-path test above is real rather than a
// fixture that could silently diverge from what CheckHostAt verifies.
func canonicalHelperBody(t *testing.T, path string) []byte {
	t.Helper()
	root := repoRoot(t)

	var file, anchor string
	switch path {
	case utils.EndpointSysctlHelper:
		file = filepath.Join(root, "scripts", "setup.sh")
		anchor = "install_endpoint_sysctl_helper()"
	case "/usr/local/lib/spinifex/ovs-socket-perms.sh":
		file = filepath.Join(root, "scripts", "setup-ovn.sh")
		anchor = "Step 8: Grant the spinifex service users access to OVS/OVN"
	default:
		t.Fatalf("canonicalHelperBody: unknown managed path %s", path)
	}

	body, err := extractHeredoc(file, anchor)
	if err != nil {
		t.Fatalf("extractHeredoc(%s, %q): %v", file, anchor, err)
	}
	return body
}
