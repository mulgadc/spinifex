package firstboot

import (
	"os"
	"os/exec"
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

	cfg := Config{Hostname: "test-node"}
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

	cfg := Config{Hostname: "test-node"}
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

	cfg := Config{Hostname: "test-node", SkipFormation: true}
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
		Hostname: "hydrogen",
		LANIP:    "10.0.0.3",
		WANIP:    "216.218.163.99",
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
}

// Without a bind address there is nothing to correct: spx auto-detects both,
// and a stray --advertise would pin the node to an address the installer only
// guessed at. A single-NIC node also collapses wan and lan onto one address,
// where the split is meaningless.
func TestBuildClusterCmdOmitsAdvertiseWithoutBind(t *testing.T) {
	cmd := buildClusterCmd(Config{
		Hostname: "node1",
		WANIP:    "216.218.163.99",
	})
	if strings.Contains(cmd, "--advertise") {
		t.Errorf("advertise must not be passed without a bind address:\n%s", cmd)
	}
}

// A DHCP wan has no address at install time, so the advertise value has to be
// resolved at boot. Shipping --bind alone here would republish the internal
// plane as the node's public dial target, which is the case the static path
// already guards against.
func TestBuildClusterCmdResolvesDHCPWanAtBoot(t *testing.T) {
	cmd := buildClusterCmd(Config{
		Hostname: "hydrogen",
		LANIP:    "10.0.0.3",
		// WANIP empty — the wan plane is on DHCP.
	})
	if !strings.Contains(cmd, "ip -4 -o addr show br-wan") {
		t.Errorf("must read the wan address off the bridge:\n%s", cmd)
	}
	if !strings.Contains(cmd, "$SPX_ADVERTISE") {
		t.Errorf("resolved address must reach the command:\n%s", cmd)
	}
	// The preamble has to run before the command that consumes it.
	if strings.Index(cmd, "SPX_ADVERTISE=\"\"") > strings.Index(cmd, "spx admin init") {
		t.Errorf("preamble must precede the spx invocation:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--bind 10.0.0.3") {
		t.Errorf("bind still comes from the lan plane:\n%s", cmd)
	}
}

// With no bind address there is nothing to correct, so neither the literal flag
// nor the boot-time lookup should appear — spx auto-detects both ends.
func TestBuildClusterCmdSkipsWanLookupWithoutBind(t *testing.T) {
	cmd := buildClusterCmd(Config{Hostname: "node1"})
	if strings.Contains(cmd, "SPX_ADVERTISE") || strings.Contains(cmd, "--advertise") {
		t.Errorf("no bind means no advertise handling at all:\n%s", cmd)
	}
}

// The advertise handling injects a shell preamble into the generated script, so
// the permutations that change its shape are syntax-checked rather than only
// string-matched — a broken quote here bricks first boot with no installer error.
func TestGeneratedScriptIsValidShell(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"dhcp wan resolves at boot", Config{Hostname: "hydrogen", LANIP: "10.0.0.3"}},
		{"static wan is inlined", Config{Hostname: "hydrogen", LANIP: "10.0.0.3", WANIP: "216.218.163.99"}},
		{"single nic, nothing pinned", Config{Hostname: "node1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			makeRootDirs(t, root)
			if err := Write(root, tc.cfg); err != nil {
				t.Fatalf("Write: %v", err)
			}
			path := filepath.Join(root, "usr/local/bin/spinifex-firstboot.sh")
			if out, err := exec.Command("bash", "-n", path).CombinedOutput(); err != nil {
				script, _ := os.ReadFile(path)
				t.Fatalf("generated script is not valid shell: %v\n%s\n---\n%s", err, out, script)
			}
		})
	}
}

// The installer cannot form a multi-node cluster: membership decides the OVN
// database topology, and the join token only exists once the primary has
// booted. Neither is knowable at install time, so firstboot must only ever
// initialize a single-node cluster and leave multi-node to a post-install
// conversion.
func TestBuildClusterCmdNeverJoins(t *testing.T) {
	for _, cfg := range []Config{
		{Hostname: "node1"},
		{Hostname: "node1", LANIP: "10.0.0.3", WANIP: "216.218.163.99"},
		{Hostname: "node1", LANIP: "10.0.0.3"},
	} {
		cmd := buildClusterCmd(cfg)
		if strings.Contains(cmd, "spx admin join") {
			t.Errorf("firstboot must never join a cluster:\n%s", cmd)
		}
		if !strings.Contains(cmd, "--nodes 1") {
			t.Errorf("want a single-node init, got:\n%s", cmd)
		}
	}
}
