package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/runtime/compute/gpu"
	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostos"
)

const testKver = "6.12.0-207.111.5.6.el10uek.x86_64"

// An lsinitrd listing trimmed from a dracut 107 image built on OL10 with UEK R8.
const testLsinitrd = `Image: /boot/initramfs-6.12.0-207.111.5.6.el10uek.x86_64.img: 28M
========================================================================
dracut modules:
kernel-modules
========================================================================
drwxr-xr-x   2 root     root            0 Aug 13 00:00 etc/cmdline.d
-rw-r--r--   1 root     root           73 Aug 13 00:00 etc/cmdline.d/20-force_drivers.conf
-rw-r--r--   1 root     root           44 Aug 13 00:00 etc/modprobe.d/blacklist-nouveau.conf
-rw-r--r--   1 root     root           41 Aug 13 00:00 etc/modprobe.d/vfio-pci.conf
lrwxrwxrwx   1 root     root            7 Aug 13 00:00 lib -> usr/lib
-rw-r--r--   1 root     root        43192 Aug 13 00:00 usr/lib/modules/6.12.0-207.111.5.6.el10uek.x86_64/kernel/drivers/vfio/pci/vfio-pci-core.ko.xz
-rw-r--r--   1 root     root         9704 Aug 13 00:00 usr/lib/modules/6.12.0-207.111.5.6.el10uek.x86_64/kernel/drivers/vfio/pci/vfio-pci.ko.xz
`

// UEK R8 builds vfio and vfio_iommu_type1 into the kernel.
const testBuiltin = "kernel/drivers/vfio/vfio.ko\nkernel/drivers/vfio/vfio_iommu_type1.ko\n"

const testForceDrivers = "rd.driver.pre=vfio\nrd.driver.pre=vfio_iommu_type1\nrd.driver.pre=vfio_pci\n"

type fakeBootHost struct {
	t       *testing.T
	root    string
	calls   []string
	runErr  map[string]error
	outputs map[string]string
}

func newFakeBootHost(t *testing.T, bootloader, initramfs string) (*fakeBootHost, *gpuBoot) {
	h := &fakeBootHost{t: t, root: t.TempDir(), runErr: map[string]error{}, outputs: map[string]string{}}
	b := &gpuBoot{
		tools: hostos.BootTools{
			Bootloader: hostos.Tool{Name: bootloader, Path: "/usr/sbin/" + bootloader},
			Initramfs:  hostos.Tool{Name: initramfs, Path: "/usr/bin/" + initramfs},
		},
		root: h.root,
		run: func(name string, args ...string) error {
			call := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
			h.calls = append(h.calls, call)
			return h.runErr[call]
		},
		output: func(name string, args ...string) ([]byte, error) {
			call := strings.Join(append([]string{filepath.Base(name)}, args...), " ")
			out, ok := h.outputs[call]
			if !ok {
				return nil, errors.New("unexpected call " + call)
			}
			return []byte(out), nil
		},
	}
	return h, b
}

func (h *fakeBootHost) write(path, content string) {
	h.t.Helper()
	full := filepath.Join(h.root, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeBootHost) read(path string) string {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.root, path))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func requireErrContains(t *testing.T, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want one naming %v", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not name %q", err, w)
		}
	}
}

func TestSetKernelArgsUpdateGrub(t *testing.T) {
	h, b := newFakeBootHost(t, "update-grub", "update-initramfs")
	h.write(grubDefaultFile, "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet\"\nGRUB_CMDLINE_LINUX=\"\"\n")

	if err := b.setKernelArgs("intel_iommu=on iommu=pt"); err != nil {
		t.Fatal(err)
	}
	if got := h.read(grubDefaultFile); !strings.Contains(got, `GRUB_CMDLINE_LINUX_DEFAULT="quiet intel_iommu=on iommu=pt"`) {
		t.Errorf("/etc/default/grub = %q", got)
	}
	if !slices.Equal(h.calls, []string{"update-grub"}) {
		t.Errorf("calls = %v, want update-grub", h.calls)
	}
}

func TestSetKernelArgsGrubby(t *testing.T) {
	const params = "amd_iommu=on iommu=pt"
	const update = "grubby --update-kernel=ALL --args=" + params
	cases := map[string]struct {
		defaultArgs string
		cmdline     string
		runErr      error
		wantErr     []string
	}{
		"applied": {
			defaultArgs: "ro crashkernel=auto amd_iommu=on iommu=pt",
			cmdline:     "ro crashkernel=auto amd_iommu=on iommu=pt\n",
		},
		"grubby fails": {
			runErr:  errors.New("exit status 1"),
			wantErr: []string{"grubby --update-kernel=ALL"},
		},
		"default entry lacks an argument": {
			defaultArgs: "ro amd_iommu=on",
			cmdline:     "ro amd_iommu=on iommu=pt\n",
			wantErr:     []string{"default boot entry", "iommu=pt"},
		},
		"new kernels would lack an argument": {
			defaultArgs: "ro amd_iommu=on iommu=pt",
			cmdline:     "ro iommu=pt\n",
			wantErr:     []string{kernelCmdlineFile, "amd_iommu=on"},
		},
		"no kernel cmdline file": {
			defaultArgs: "ro amd_iommu=on iommu=pt",
			wantErr:     []string{kernelCmdlineFile},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, b := newFakeBootHost(t, "grubby", "dracut")
			h.runErr[update] = tc.runErr
			h.outputs["grubby --info=DEFAULT"] = "index=0\nkernel=\"/boot/vmlinuz-" + testKver + "\"\nargs=\"" + tc.defaultArgs + "\"\nroot=\"UUID=abc\"\n"
			if tc.cmdline != "" {
				h.write(kernelCmdlineFile, tc.cmdline)
			}

			err := b.setKernelArgs(params)
			if tc.wantErr != nil {
				requireErrContains(t, err, tc.wantErr...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(h.calls, []string{update}) {
				t.Errorf("calls = %v, want %q", h.calls, update)
			}
		})
	}
}

func TestConfigureEarlyVFIODracut(t *testing.T) {
	h, b := newFakeBootHost(t, "grubby", "dracut")
	files := []string{vfioPCIConfName, nouveauConfName}

	changed, err := b.configureEarlyVFIO(files)
	if err != nil || !changed {
		t.Fatalf("first run: changed=%v err=%v, want a write", changed, err)
	}
	conf := h.read(dracutVFIOConf)
	for _, want := range []string{
		`force_drivers+=" vfio vfio_iommu_type1 vfio_pci "`,
		`install_items+=" /etc/modprobe.d/vfio-pci.conf /etc/modprobe.d/blacklist-nouveau.conf "`,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("%s lacks %s:\n%s", dracutVFIOConf, want, conf)
		}
	}
	if strings.Contains(conf, "add_drivers") {
		t.Errorf("%s uses add_drivers, which does not load the module early:\n%s", dracutVFIOConf, conf)
	}

	if changed, err := b.configureEarlyVFIO(files); err != nil || changed {
		t.Fatalf("rerun: changed=%v err=%v, want no change", changed, err)
	}
	if changed, err := b.configureEarlyVFIO(append(files, amdgpuConfName)); err != nil || !changed {
		t.Fatalf("with the amdgpu blacklist: changed=%v err=%v, want a rewrite", changed, err)
	}
	if !strings.Contains(h.read(dracutVFIOConf), "/etc/modprobe.d/blacklist-amdgpu.conf") {
		t.Errorf("rewrite lacks the amdgpu blacklist")
	}
	if _, err := os.Stat(filepath.Join(h.root, initramfsModules)); !os.IsNotExist(err) {
		t.Errorf("dracut host got %s", initramfsModules)
	}
}

func TestConfigureEarlyVFIOInitramfsTools(t *testing.T) {
	h, b := newFakeBootHost(t, "update-grub", "update-initramfs")
	h.write(initramfsModules, "# List of modules\n")

	if changed, err := b.configureEarlyVFIO(nil); err != nil || !changed {
		t.Fatalf("first run: changed=%v err=%v, want a write", changed, err)
	}
	if changed, err := b.configureEarlyVFIO(nil); err != nil || changed {
		t.Fatalf("rerun: changed=%v err=%v, want no change", changed, err)
	}
	if got := h.read(initramfsModules); strings.Count(got, "vfio_pci") != 1 || !strings.Contains(got, "vfio\nvfio_iommu_type1\nvfio_pci\n") {
		t.Errorf("%s = %q", initramfsModules, got)
	}
	if _, err := os.Stat(filepath.Join(h.root, dracutVFIOConf)); !os.IsNotExist(err) {
		t.Errorf("initramfs-tools host got %s", dracutVFIOConf)
	}
}

func TestRebuildInitramfsUpdateInitramfs(t *testing.T) {
	h, b := newFakeBootHost(t, "update-grub", "update-initramfs")
	if err := b.rebuildInitramfs(nil); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(h.calls, []string{"update-initramfs -u"}) {
		t.Errorf("calls = %v", h.calls)
	}
}

func TestRebuildInitramfsDracut(t *testing.T) {
	files := []string{vfioPCIConfName, nouveauConfName}
	cases := map[string]struct {
		listing      string
		builtin      string
		forceDrivers string
		files        []string
		wantErr      []string
	}{
		"complete image": {listing: testLsinitrd, builtin: testBuiltin, forceDrivers: testForceDrivers},
		"every vfio module as a file": {
			listing: testLsinitrd +
				"-rw-r--r--   1 root     root        1 Aug 13 00:00 usr/lib/modules/x/kernel/drivers/vfio/vfio.ko.zst\n" +
				"-rw-r--r--   1 root     root        1 Aug 13 00:00 usr/lib/modules/x/kernel/drivers/vfio/vfio_iommu_type1.ko\n",
			forceDrivers: testForceDrivers,
		},
		"no vfio-pci module": {
			listing:      strings.ReplaceAll(testLsinitrd, "/vfio-pci.ko.xz", "/other.ko.xz"),
			builtin:      testBuiltin,
			forceDrivers: testForceDrivers,
			wantErr:      []string{testKver, "module vfio_pci"},
		},
		"vfio core neither built in nor in the image": {
			listing:      testLsinitrd,
			forceDrivers: testForceDrivers,
			wantErr:      []string{"module vfio,", "module vfio_iommu_type1"},
		},
		"no vfio-pci options": {
			listing:      strings.ReplaceAll(testLsinitrd, "etc/modprobe.d/vfio-pci.conf", "etc/modprobe.d/other.conf"),
			builtin:      testBuiltin,
			forceDrivers: testForceDrivers,
			wantErr:      []string{"/etc/modprobe.d/vfio-pci.conf"},
		},
		"no amdgpu blacklist": {
			listing:      testLsinitrd,
			builtin:      testBuiltin,
			forceDrivers: testForceDrivers,
			files:        append(slices.Clone(files), amdgpuConfName),
			wantErr:      []string{"/etc/modprobe.d/blacklist-amdgpu.conf"},
		},
		"vfio-pci not loaded early": {
			listing:      testLsinitrd,
			builtin:      testBuiltin,
			forceDrivers: "rd.driver.pre=vfio\n",
			wantErr:      []string{"rd.driver.pre=vfio_pci"},
		},
		// lsinitrd -f prints nothing and exits 0 when the file is absent.
		"no force_drivers file": {
			listing: testLsinitrd,
			builtin: testBuiltin,
			wantErr: []string{"rd.driver.pre=vfio_pci"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, b := newFakeBootHost(t, "grubby", "dracut")
			h.write(filepath.Join("/lib/modules", testKver, "modules.dep"), "")
			h.write(filepath.Join("/lib/modules", testKver, "modules.builtin"), tc.builtin)
			h.outputs["lsinitrd -k "+testKver] = tc.listing
			h.outputs["lsinitrd -k "+testKver+" -f "+dracutForceDrivers] = tc.forceDrivers
			want := files
			if tc.files != nil {
				want = tc.files
			}

			err := b.rebuildInitramfs(want)
			if !slices.Equal(h.calls, []string{"dracut -f --regenerate-all"}) {
				t.Errorf("calls = %v", h.calls)
			}
			if tc.wantErr != nil {
				requireErrContains(t, err, tc.wantErr...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRebuildInitramfsDracutChecksEveryKernel(t *testing.T) {
	const older = "6.12.0-200.1.el10uek.x86_64"
	h, b := newFakeBootHost(t, "grubby", "dracut")
	for _, k := range []string{testKver, older} {
		h.write(filepath.Join("/lib/modules", k, "modules.dep"), "")
		h.write(filepath.Join("/lib/modules", k, "modules.builtin"), testBuiltin)
		h.outputs["lsinitrd -k "+k+" -f "+dracutForceDrivers] = testForceDrivers
	}
	// A modules directory left behind by a removed kernel has no modules.dep.
	h.write("/lib/modules/6.11.0-old/extra/leftover.ko", "")
	h.outputs["lsinitrd -k "+testKver] = testLsinitrd
	h.outputs["lsinitrd -k "+older] = strings.ReplaceAll(testLsinitrd, "/vfio-pci.ko.xz", "/other.ko.xz")

	requireErrContains(t, b.rebuildInitramfs([]string{vfioPCIConfName}), older, "module vfio_pci")
}

func TestRebuildInitramfsDracutFails(t *testing.T) {
	h, b := newFakeBootHost(t, "grubby", "dracut")
	h.runErr["dracut -f --regenerate-all"] = errors.New("exit status 1")
	requireErrContains(t, b.rebuildInitramfs(nil), "dracut -f --regenerate-all")
}

func TestGPUSetupAddGRUBParams(t *testing.T) {
	cases := map[string]struct {
		grub        string
		wantChanged bool
		wantErr     bool
		want        string
	}{
		"appends":         {grub: "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet\"\n", wantChanged: true, want: "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet amd_iommu=on iommu=pt\"\n"},
		"already present": {grub: "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet amd_iommu=on\"\n", want: "GRUB_CMDLINE_LINUX_DEFAULT=\"quiet amd_iommu=on\"\n"},
		"no DEFAULT line": {grub: "GRUB_CMDLINE_LINUX=\"rhgb\"\n", wantErr: true, want: "GRUB_CMDLINE_LINUX=\"rhgb\"\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "grub")
			if err := os.WriteFile(path, []byte(tc.grub), 0o644); err != nil {
				t.Fatal(err)
			}
			changed, err := gpuSetupAddGRUBParams(path, "amd_iommu=on iommu=pt")
			if (err != nil) != tc.wantErr || changed != tc.wantChanged {
				t.Fatalf("changed=%v err=%v, want changed=%v error=%v", changed, err, tc.wantChanged, tc.wantErr)
			}
			if got, _ := os.ReadFile(path); string(got) != tc.want {
				t.Errorf("file = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGPUSlotFunctions(t *testing.T) {
	sys := t.TempDir()
	for _, addr := range []string{"0000:03:00.0", "0000:03:00.1", "0000:03:01.0", "0000:04:00.0"} {
		if err := os.MkdirAll(filepath.Join(sys, "bus/pci/devices", addr), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := gpuSlotFunctions(sys, "0000:03:00.0"); !slices.Equal(got, []string{"0000:03:00.0", "0000:03:00.1"}) {
		t.Errorf("got %v, want the two functions of slot 03:00", got)
	}
}

func TestGPUSetupVerifyBindings(t *testing.T) {
	devices := []gpu.GPUDevice{{PCIAddress: "0000:03:00.0"}}
	functions := func(string) []string { return []string{"0000:03:00.0", "0000:03:00.1"} }
	cases := map[string]struct {
		drivers   map[string]string
		bindErr   error
		wantBound []string
		wantErr   []string
	}{
		"all on vfio-pci": {drivers: map[string]string{"0000:03:00.0": "vfio-pci", "0000:03:00.1": "vfio-pci"}},
		"unbound sibling is bound": {
			drivers:   map[string]string{"0000:03:00.0": "vfio-pci", "0000:03:00.1": ""},
			wantBound: []string{"0000:03:00.1"},
		},
		"sibling on a host driver": {
			drivers: map[string]string{"0000:03:00.0": "vfio-pci", "0000:03:00.1": "snd_hda_intel"},
			wantErr: []string{"0000:03:00.1", "snd_hda_intel"},
		},
		"GPU on its native driver": {
			drivers: map[string]string{"0000:03:00.0": "nvidia", "0000:03:00.1": "vfio-pci"},
			wantErr: []string{"0000:03:00.0", "nvidia"},
		},
		"bind fails": {
			drivers: map[string]string{"0000:03:00.0": "", "0000:03:00.1": "vfio-pci"},
			bindErr: errors.New("permission denied"),
			wantErr: []string{"0000:03:00.0", "permission denied"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var bound []string
			err := gpuSetupVerifyBindings(devices, functions,
				func(addr string) (string, error) { return tc.drivers[addr], nil },
				func(addr string) error { bound = append(bound, addr); return tc.bindErr })
			if tc.wantErr != nil {
				requireErrContains(t, err, tc.wantErr...)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(bound, tc.wantBound) {
				t.Errorf("bound %v, want %v", bound, tc.wantBound)
			}
		})
	}
}
