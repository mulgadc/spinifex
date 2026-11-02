package hostos

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// fakeHost is a temp root plus a PATH made of a name-to-path map, counting
// lookups so the caching tests can see whether the host was probed again.
type fakeHost struct {
	root    string
	path    map[string]string
	lookups int
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	return &fakeHost{root: t.TempDir(), path: map[string]string{}}
}

func (h *fakeHost) resolver(hostArch string) *resolver {
	r := newResolver()
	r.root = h.root
	r.hostArch = hostArch
	r.lookPath = func(name string) (string, error) {
		h.lookups++
		if p, ok := h.path[name]; ok {
			return p, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	return r
}

func (h *fakeHost) onPath(names ...string) {
	for _, n := range names {
		h.path[n] = "/usr/bin/" + n
	}
}

func (h *fakeHost) installQEMUKVM(t *testing.T, mode os.FileMode) {
	t.Helper()
	h.install(t, QEMUKVMPath, mode)
}

func (h *fakeHost) install(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(h.root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, mode); err != nil {
		t.Fatal(err)
	}
}

// requireErrNames fails unless err is non-nil and mentions every word.
func requireErrNames(t *testing.T, err error, words ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error naming %v, got nil", words)
	}
	for _, w := range words {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error does not name %q: %v", w, err)
		}
	}
}

func TestQEMUBinary_QEMUKVMWinsOverQEMUSystemOnPath(t *testing.T) {
	h := newFakeHost(t)
	h.installQEMUKVM(t, 0o755)
	h.onPath("qemu-system-x86_64")

	got, err := h.resolver("x86_64").qemuBinary("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if got != QEMUKVMPath {
		t.Fatalf("got %q, want %q", got, QEMUKVMPath)
	}
}

func TestQEMUBinary_QEMUSystemWithoutQEMUKVM(t *testing.T) {
	h := newFakeHost(t)
	h.onPath("qemu-system-x86_64")

	got, err := h.resolver("x86_64").qemuBinary("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/qemu-system-x86_64" {
		t.Fatalf("got %q, want the qemu-system-x86_64 on PATH", got)
	}
}

func TestQEMUBinary_NonExecutableQEMUKVMIsIgnored(t *testing.T) {
	h := newFakeHost(t)
	h.installQEMUKVM(t, 0o644)
	h.onPath("qemu-system-x86_64")

	got, err := h.resolver("x86_64").qemuBinary("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/qemu-system-x86_64" {
		t.Fatalf("got %q, want the qemu-system-x86_64 on PATH", got)
	}
}

func TestQEMUBinary_NeitherNamesBothCandidatesAndPackages(t *testing.T) {
	h := newFakeHost(t)

	_, err := h.resolver("x86_64").qemuBinary("x86_64")
	requireErrNames(t, err, QEMUKVMPath, "qemu-kvm", "qemu-system-x86_64", "qemu-system-x86")
}

func TestQEMUBinary_ForeignArchIsNeverQEMUKVM(t *testing.T) {
	h := newFakeHost(t)
	h.installQEMUKVM(t, 0o755)

	_, err := h.resolver("x86_64").qemuBinary("arm64")
	requireErrNames(t, err, QEMUKVMPath, "runs only x86_64 guests", "qemu-system-aarch64", "qemu-system-arm")
}

func TestQEMUBinary_ForeignArchUsesQEMUSystemBesideQEMUKVM(t *testing.T) {
	h := newFakeHost(t)
	h.installQEMUKVM(t, 0o755)
	h.onPath("qemu-system-aarch64")

	got, err := h.resolver("x86_64").qemuBinary("arm64")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/usr/bin/qemu-system-aarch64" {
		t.Fatalf("got %q, want the qemu-system-aarch64 on PATH", got)
	}
}

func TestQEMUBinary_ForeignArchWithoutQEMUKVMNamesOnlyQEMUSystem(t *testing.T) {
	h := newFakeHost(t)

	_, err := h.resolver("x86_64").qemuBinary("arm64")
	requireErrNames(t, err, "qemu-system-aarch64", "qemu-system-arm")
	if strings.Contains(err.Error(), QEMUKVMPath) {
		t.Errorf("error offers qemu-kvm, which cannot run arm64 guests on x86_64: %v", err)
	}
}

func TestQEMUBinary_UnknownArch(t *testing.T) {
	h := newFakeHost(t)
	h.onPath("qemu-system-riscv64")

	_, err := h.resolver("x86_64").qemuBinary("riscv64")
	requireErrNames(t, err, `"riscv64"`, "x86_64", "arm64")
}

func TestQEMUBinary_CachesSuccessPerArch(t *testing.T) {
	h := newFakeHost(t)
	h.onPath("qemu-system-x86_64", "qemu-system-aarch64")
	r := h.resolver("x86_64")

	first, err := r.qemuBinary("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	lookups := h.lookups
	h.installQEMUKVM(t, 0o755)

	again, err := r.qemuBinary("x86_64")
	if err != nil {
		t.Fatal(err)
	}
	if again != first || h.lookups != lookups {
		t.Fatalf("second call re-probed: got %q (first %q), lookups %d -> %d", again, first, lookups, h.lookups)
	}

	other, err := r.qemuBinary("arm64")
	if err != nil {
		t.Fatal(err)
	}
	if other != "/usr/bin/qemu-system-aarch64" {
		t.Fatalf("arm64 got %q from the x86_64 cache entry", other)
	}
}

func TestQEMUBinary_FailureIsNotCached(t *testing.T) {
	h := newFakeHost(t)
	r := h.resolver("x86_64")

	if _, err := r.qemuBinary("x86_64"); err == nil {
		t.Fatal("want an error before QEMU is installed")
	}
	h.installQEMUKVM(t, 0o755)

	got, err := r.qemuBinary("x86_64")
	if err != nil {
		t.Fatalf("QEMU installed after a failed lookup is not found: %v", err)
	}
	if got != QEMUKVMPath {
		t.Fatalf("got %q, want %q", got, QEMUKVMPath)
	}
}

func TestHostArch(t *testing.T) {
	for goarch, want := range map[string]string{"amd64": "x86_64", "arm64": "arm64"} {
		if got := hostArch(goarch); got != want {
			t.Errorf("hostArch(%q) = %q, want %q", goarch, got, want)
		}
	}
}

func TestSystemCATrust(t *testing.T) {
	cases := []struct {
		name      string
		onPath    []string
		wantTool  string
		wantDir   string
		wantError []string
	}{
		{name: "debian", onPath: []string{"update-ca-certificates"}, wantTool: "update-ca-certificates", wantDir: "/usr/local/share/ca-certificates"},
		{name: "el", onPath: []string{"update-ca-trust"}, wantTool: "update-ca-trust", wantDir: "/etc/pki/ca-trust/source/anchors"},
		{name: "both", onPath: []string{"update-ca-certificates", "update-ca-trust"}, wantError: []string{"/usr/bin/update-ca-certificates", "/usr/bin/update-ca-trust"}},
		{name: "neither", wantError: []string{"update-ca-certificates", "update-ca-trust"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.onPath(tc.onPath...)

			got, err := h.resolver("x86_64").caTrust()
			if tc.wantError != nil {
				requireErrNames(t, err, tc.wantError...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Update.Name != tc.wantTool || got.Update.Path != "/usr/bin/"+tc.wantTool || got.AnchorDir != tc.wantDir {
				t.Fatalf("got %+v, want %s in %s", got, tc.wantTool, tc.wantDir)
			}
		})
	}
}

func TestSystemCATrust_CachesSuccess(t *testing.T) {
	h := newFakeHost(t)
	h.onPath("update-ca-certificates")
	r := h.resolver("x86_64")

	if _, err := r.caTrust(); err != nil {
		t.Fatal(err)
	}
	h.onPath("update-ca-trust")

	got, err := r.caTrust()
	if err != nil {
		t.Fatalf("cached result was re-probed: %v", err)
	}
	if got.Update.Name != "update-ca-certificates" {
		t.Fatalf("got %q, want the cached update-ca-certificates", got.Update.Name)
	}
}

func TestSystemBootTools(t *testing.T) {
	cases := []struct {
		name           string
		onPath         []string
		wantBootloader string
		wantInitramfs  string
		wantError      []string
	}{
		{name: "debian", onPath: []string{"update-grub", "update-initramfs"}, wantBootloader: "update-grub", wantInitramfs: "update-initramfs"},
		{name: "el", onPath: []string{"grubby", "dracut"}, wantBootloader: "grubby", wantInitramfs: "dracut"},
		{name: "both bootloader tools", onPath: []string{"update-grub", "grubby", "dracut"}, wantError: []string{"update-grub", "grubby"}},
		{name: "both initramfs tools", onPath: []string{"grubby", "update-initramfs", "dracut"}, wantError: []string{"update-initramfs", "dracut"}},
		{name: "no bootloader tool", onPath: []string{"dracut"}, wantError: []string{"update-grub", "grubby"}},
		{name: "no initramfs tool", onPath: []string{"grubby"}, wantError: []string{"update-initramfs", "dracut"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			h.onPath(tc.onPath...)

			got, err := h.resolver("x86_64").bootTools()
			if tc.wantError != nil {
				requireErrNames(t, err, tc.wantError...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Bootloader.Name != tc.wantBootloader || got.Initramfs.Name != tc.wantInitramfs {
				t.Fatalf("got %+v, want %s and %s", got, tc.wantBootloader, tc.wantInitramfs)
			}
		})
	}
}

// strongSwan's charon is not pluto, so only libreswan's own daemon counts, and
// a pluto that cannot run is not an IPsec stack.
func TestHasLibreswan(t *testing.T) {
	for _, tc := range []struct {
		name    string
		install map[string]os.FileMode
		want    bool
	}{
		{name: "pluto", install: map[string]os.FileMode{LibreswanPlutoPath: 0o755}, want: true},
		{name: "strongSwan charon only", install: map[string]os.FileMode{"/usr/lib/ipsec/charon": 0o755}},
		{name: "pluto not executable", install: map[string]os.FileMode{LibreswanPlutoPath: 0o644}},
		{name: "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t)
			for path, mode := range tc.install {
				h.install(t, path, mode)
			}
			if got := h.resolver("x86_64").hasLibreswan(); got != tc.want {
				t.Fatalf("hasLibreswan() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsQEMUProcess(t *testing.T) {
	for comm, want := range map[string]bool{
		// comm truncates to 15 bytes, so qemu-system-x86_64 reads qemu-system-x86.
		"qemu-system-x86\n": true,
		"qemu-system-aar":   true,
		"qemu-kvm\n":        true,
		"qemu-img":          false,
		"qemu-kvm-helper":   false,
		"qemu-nbd":          false,
		"nbdkit":            false,
		"":                  false,
	} {
		if got := IsQEMUProcess(comm); got != want {
			t.Errorf("IsQEMUProcess(%q) = %v, want %v", comm, got, want)
		}
	}
}

func TestQEMUProcessPatternMatchesEveryQEMUName(t *testing.T) {
	re := regexp.MustCompile(QEMUProcessPattern)
	for _, line := range []string{
		"spinifex 4242 qemu-system-x86_64 -name guest=i-0abc,debug-threads=on",
		"spinifex 4243 /usr/libexec/qemu-kvm -name guest=i-0def,debug-threads=on",
	} {
		if !re.MatchString(line) {
			t.Errorf("QEMUProcessPattern %q does not match %q", QEMUProcessPattern, line)
		}
	}
	if re.MatchString("spinifex 4244 nbdkit --unix /run/spinifex/nbd.sock") {
		t.Errorf("QEMUProcessPattern %q matches an nbdkit line", QEMUProcessPattern)
	}
}
