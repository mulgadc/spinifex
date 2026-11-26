package preflight

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

const (
	binLabel  = "system_u:object_r:bin_t:s0"
	swapLabel = "system_u:object_r:swapfile_t:s0"
)

// selinuxHost is one host state: the enforce file, the config, the module
// store entry and the labels fileLabel returns, keyed by host path.
type selinuxHost struct {
	enforce string // "" leaves selinuxfs unmounted
	config  string // "" writes no /etc/selinux/config
	module  string // policy type whose store holds the module; "" for none
	extra   []string // more store paths, relative to that type's modules dir
	labels  map[string]string
}

func (h selinuxHost) build(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, body string) {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if h.enforce != "" {
		write(selinuxEnforce, h.enforce)
	}
	if h.config != "" {
		write(selinuxConfig, h.config)
	}
	if h.module != "" {
		store := fmt.Sprintf(selinuxModulesDir, h.module)
		if err := os.MkdirAll(filepath.Join(root, store, "300", selinuxModule), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, p := range h.extra {
			write(filepath.Join(store, p), "")
		}
	}
	orig := fileLabel
	t.Cleanup(func() { fileLabel = orig })
	fileLabel = func(path string) (string, error) {
		if l, ok := h.labels[strings.TrimPrefix(path, root)]; ok {
			return l, nil
		}
		return "", unix.ENOENT
	}
	return root
}

func scriptLabels(extra map[string]string) map[string]string {
	l := map[string]string{
		"/var/lib/spinifex/wait-for-nats.sh":    binLabel,
		"/var/lib/spinifex/nats-js-watchdog.sh": binLabel,
	}
	maps.Copy(l, extra)
	return l
}

func selinuxResults(results []Result) []Result {
	var out []Result
	for _, r := range results {
		if r.Kind == "selinux" {
			out = append(out, r)
		}
	}
	return out
}

func TestCheckHostAt_SELinuxSkippedWithoutSELinux(t *testing.T) {
	root := selinuxHost{}.build(t)
	if got := selinuxResults(CheckHostAt(root)); len(got) != 0 {
		t.Fatalf("a host with no SELinux got selinux results: %+v", got)
	}
}

func TestCheckHostAt_SELinuxDisabledWarns(t *testing.T) {
	root := selinuxHost{config: "SELINUX=disabled\n"}.build(t)
	r := findResult(t, CheckHostAt(root), selinuxConfig)
	if r.Status != Warning || !strings.Contains(r.Detail, "disabled") {
		t.Fatalf("got %s %q, want a disabled warning", r.Status, r.Detail)
	}
}

func TestCheckHostAt_SELinux(t *testing.T) {
	const cfg = "SELINUX=enforcing\nSELINUXTYPE=targeted\n"
	for name, tc := range map[string]struct {
		host    selinuxHost
		path    string
		want    Status
		detail  string
		problem bool
	}{
		"enforcing, all correct": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", labels: scriptLabels(nil)},
			path: selinuxEnforce, want: OK,
		},
		"permissive, all correct": {
			host: selinuxHost{enforce: "0\n", config: cfg, module: "targeted", labels: scriptLabels(nil)},
			path: selinuxEnforce, want: Warning, detail: "setenforce 1",
		},
		"module missing": {
			host: selinuxHost{enforce: "1", config: cfg, labels: scriptLabels(nil)},
			path: fmt.Sprintf(selinuxModulesDir, "targeted"), want: Failed, detail: "not loaded", problem: true,
		},
		"module loaded": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", extra: []string{"100/base/cil"}, labels: scriptLabels(nil)},
			path: fmt.Sprintf(selinuxModulesDir, "targeted"), want: OK,
		},
		"module disabled": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", extra: []string{"disabled/spinifex"}, labels: scriptLabels(nil)},
			path: fmt.Sprintf(selinuxModulesDir, "targeted"), want: Failed, detail: "semodule -e spinifex", problem: true,
		},
		"module overridden at a higher priority": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", extra: []string{"400/spinifex/cil"}, labels: scriptLabels(nil)},
			path: fmt.Sprintf(selinuxModulesDir, "targeted"), want: Failed, detail: "priority 400", problem: true,
		},
		"a lower-priority copy does not override": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", extra: []string{"200/spinifex/cil"}, labels: scriptLabels(nil)},
			path: fmt.Sprintf(selinuxModulesDir, "targeted"), want: OK,
		},
		"module looked up under the configured policy type": {
			host: selinuxHost{enforce: "1", config: "SELINUXTYPE=\"mls\"\n", module: "targeted", labels: scriptLabels(nil)},
			path: fmt.Sprintf(selinuxModulesDir, "mls"), want: Failed, detail: "not loaded", problem: true,
		},
		"script left var_lib_t": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", labels: scriptLabels(map[string]string{
				"/var/lib/spinifex/wait-for-nats.sh": "system_u:object_r:var_lib_t:s0",
			})},
			path: "/var/lib/spinifex/wait-for-nats.sh", want: Failed, detail: "restorecon -F /var/lib/spinifex/wait-for-nats.sh", problem: true,
		},
		"script missing": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", labels: map[string]string{
				"/var/lib/spinifex/wait-for-nats.sh": binLabel,
			}},
			path: "/var/lib/spinifex/nats-js-watchdog.sh", want: Missing, problem: true,
		},
		"swapfile labelled": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", labels: scriptLabels(map[string]string{"/swapfile": swapLabel})},
			path: "/swapfile", want: OK,
		},
		"swapfile mislabelled": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", labels: scriptLabels(map[string]string{"/swapfile": "system_u:object_r:etc_runtime_t:s0"})},
			path: "/swapfile", want: Failed, detail: "want type swapfile_t", problem: true,
		},
		"label with a category range": {
			host: selinuxHost{enforce: "1", config: cfg, module: "targeted", labels: scriptLabels(map[string]string{"/etc/ipsec.secrets": "system_u:object_r:ipsec_key_file_t:s0:c0.c1023"})},
			path: "/etc/ipsec.secrets", want: OK,
		},
	} {
		t.Run(name, func(t *testing.T) {
			results := CheckHostAt(tc.host.build(t))
			r := findResult(t, results, tc.path)
			if r.Status != tc.want {
				t.Fatalf("status = %s %q, want %s", r.Status, r.Detail, tc.want)
			}
			if !strings.Contains(r.Detail, tc.detail) {
				t.Errorf("detail %q does not contain %q", r.Detail, tc.detail)
			}
			if got := HasProblem(selinuxResults(results)); got != tc.problem {
				t.Errorf("HasProblem = %v, want %v: %+v", got, tc.problem, selinuxResults(results))
			}
		})
	}
}

func TestCheckHostAt_SELinuxOptionalPathsAbsent(t *testing.T) {
	root := selinuxHost{enforce: "1", config: "SELINUX=enforcing\n", module: "targeted", labels: scriptLabels(nil)}.build(t)
	for _, r := range selinuxResults(CheckHostAt(root)) {
		if r.Path == "/swapfile" || r.Path == "/etc/ipsec.secrets" {
			t.Errorf("an absent optional path got a result: %+v", r)
		}
	}
}
