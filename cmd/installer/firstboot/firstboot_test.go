package firstboot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeRootDirs(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{
		"usr/local/bin",
		"etc/systemd/system",
		"etc/systemd/system/multi-user.target.wants",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
}

func TestWriteScriptNoCallbackWhenEmpty(t *testing.T) {
	root := t.TempDir()
	makeRootDirs(t, root)

	cfg := Config{Hostname: "test-node", ClusterRole: "init"}
	if err := Write(root, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	script, err := os.ReadFile(filepath.Join(root, "usr/local/bin/spinifex-firstboot.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if strings.Contains(string(script), "curl") {
		t.Error("script should not contain curl when InstallCallback is empty")
	}
}

func TestWriteScriptEmbedsCurlWhenCallbackSet(t *testing.T) {
	root := t.TempDir()
	makeRootDirs(t, root)

	const callbackURL = "http://192.168.1.12/boot/done?mac=aa:bb:cc:dd:ee:ff"
	cfg := Config{
		Hostname:        "test-node",
		ClusterRole:     "init",
		InstallCallback: callbackURL,
	}
	if err := Write(root, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	script, err := os.ReadFile(filepath.Join(root, "usr/local/bin/spinifex-firstboot.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	content := string(script)

	if !strings.Contains(content, "curl") {
		t.Error("script missing curl command")
	}
	if !strings.Contains(content, callbackURL) {
		t.Errorf("script missing callback URL %q", callbackURL)
	}
}

func TestWriteScriptRunsOVNWhenFormationOwned(t *testing.T) {
	root := t.TempDir()
	makeRootDirs(t, root)

	cfg := Config{Hostname: "test-node", ClusterRole: "init"}
	if err := Write(root, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	content := readScript(t, root)

	if !strings.Contains(content, "setup-ovn.sh --management") {
		t.Error("script should run setup-ovn --management when firstboot owns formation")
	}
	if !strings.Contains(content, "systemctl start ovn-central") {
		t.Error("script should pre-start ovn-central when firstboot owns formation")
	}
}

func TestWriteScriptDefersOVNWhenSkipFormation(t *testing.T) {
	root := t.TempDir()
	makeRootDirs(t, root)

	cfg := Config{Hostname: "test-node", ClusterRole: "init", SkipFormation: true}
	if err := Write(root, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	content := readScript(t, root)

	if strings.Contains(content, "setup-ovn.sh --management") {
		t.Error("script must not run setup-ovn --management when a controller owns OVN")
	}
	if strings.Contains(content, "systemctl start ovn-central") {
		t.Error("script must not pre-start ovn-central when a controller owns OVN")
	}
	if !strings.Contains(content, "setup-ovn deferred") {
		t.Error("script should note setup-ovn is deferred under SkipFormation")
	}
}

func readScript(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "usr/local/bin/spinifex-firstboot.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	return string(b)
}

func TestWriteScriptCallbackAfterDoneMarker(t *testing.T) {
	root := t.TempDir()
	makeRootDirs(t, root)

	const callbackURL = "http://192.168.1.12/boot/done?mac=aa:bb:cc:dd:ee:ff"
	cfg := Config{
		Hostname:        "node1",
		ClusterRole:     "init",
		InstallCallback: callbackURL,
	}
	if err := Write(root, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}

	script, err := os.ReadFile(filepath.Join(root, "usr/local/bin/spinifex-firstboot.sh"))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	content := string(script)

	doneIdx := strings.Index(content, "touch \"$DONE_MARKER\"")
	curlIdx := strings.Index(content, "curl")
	if doneIdx < 0 {
		t.Fatal("done marker not found in script")
	}
	if curlIdx < 0 {
		t.Fatal("curl not found in script")
	}
	if curlIdx < doneIdx {
		t.Error("curl must appear after done marker write")
	}
}

// A multi-NIC node must keep cluster traffic on the internal plane while still
// publishing the public one. --advertise has to be explicit to do that: spx
// returns a concrete --bind verbatim as the advertise address and never reaches
// its WAN auto-detection, so omitting it moves northstar's :53 listener and the
// off-host dial target onto the internal plane.
func TestBuildClusterCmdSeparatesBindFromAdvertise(t *testing.T) {
	cmd := buildClusterCmd(Config{
		Hostname:    "hydrogen",
		ClusterRole: "init",
		LANIP:       "10.0.0.3",
		WANIP:       "216.218.163.99",
	})
	for _, want := range []string{
		"--bind 10.0.0.3",
		"--cluster-bind 10.0.0.3",
		"--advertise 216.218.163.99",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("missing %q in:\n%s", want, cmd)
		}
	}

	// Joining nodes carry the same split — peers record the public address.
	cmd = buildClusterCmd(Config{
		Hostname:    "radon",
		ClusterRole: "join",
		JoinAddr:    "10.0.0.2:4432",
		LANIP:       "10.0.0.4",
		WANIP:       "216.218.163.100",
	})
	if !strings.Contains(cmd, "--advertise 216.218.163.100") {
		t.Errorf("join must advertise the public plane:\n%s", cmd)
	}
}

// Without a bind address there is nothing to correct: spx auto-detects both,
// and a stray --advertise would pin the node to an address the installer only
// guessed at. A single-NIC node also collapses wan and lan onto one address,
// where the split is meaningless.
func TestBuildClusterCmdOmitsAdvertiseWithoutBind(t *testing.T) {
	cmd := buildClusterCmd(Config{
		Hostname:    "node1",
		ClusterRole: "init",
		WANIP:       "216.218.163.99",
	})
	if strings.Contains(cmd, "--advertise") {
		t.Errorf("advertise must not be passed without a bind address:\n%s", cmd)
	}
}
