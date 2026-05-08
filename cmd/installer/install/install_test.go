package install

import (
	"os"
	"testing"
)

func TestToFirstbootConfigIncludesInstallCallback(t *testing.T) {
	const url = "http://192.168.1.12/boot/done?mac=aa:bb:cc:dd:ee:ff"
	t.Setenv("SPINIFEX_INSTALL_CALLBACK", url)

	cfg := &Config{Hostname: "node1", ClusterRole: "init"}
	fb := cfg.toFirstbootConfig()
	if fb.InstallCallback != url {
		t.Errorf("InstallCallback = %q, want %q", fb.InstallCallback, url)
	}
}

func TestToFirstbootConfigEmptyCallbackWhenEnvUnset(t *testing.T) {
	os.Unsetenv("SPINIFEX_INSTALL_CALLBACK")

	cfg := &Config{Hostname: "node1", ClusterRole: "init"}
	fb := cfg.toFirstbootConfig()
	if fb.InstallCallback != "" {
		t.Errorf("InstallCallback = %q, want empty", fb.InstallCallback)
	}
}
