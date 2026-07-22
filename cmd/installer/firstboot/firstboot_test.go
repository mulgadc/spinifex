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

func TestWriteScriptRunsHostDNSAfterFormation(t *testing.T) {
	root := t.TempDir()
	makeRootDirs(t, root)

	cfg := Config{Hostname: "test-node", ClusterRole: "init"}
	if err := Write(root, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	content := readScript(t, root)

	dnsIdx := strings.Index(content, "SETUP_STAGES=resolved /usr/local/share/spinifex/setup.sh")
	if dnsIdx < 0 {
		t.Fatal("script should run the resolved (host DNS) stage when firstboot owns formation")
	}
	// northstar.toml only exists once formation runs, so host DNS must follow it.
	formationIdx := strings.Index(content, "spx admin init")
	if formationIdx < 0 {
		t.Fatal("formation command not found in script")
	}
	if dnsIdx < formationIdx {
		t.Error("host DNS stage must run after cluster formation writes northstar.toml")
	}
}

func TestWriteScriptDefersHostDNSWhenSkipFormation(t *testing.T) {
	root := t.TempDir()
	makeRootDirs(t, root)

	cfg := Config{Hostname: "test-node", ClusterRole: "init", SkipFormation: true}
	if err := Write(root, cfg); err != nil {
		t.Fatalf("Write: %v", err)
	}
	content := readScript(t, root)

	if strings.Contains(content, "SETUP_STAGES=resolved") {
		t.Error("script must not run the host DNS stage when a controller owns formation")
	}
	if !strings.Contains(content, "host DNS deferred") {
		t.Error("script should note host DNS is deferred under SkipFormation")
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
