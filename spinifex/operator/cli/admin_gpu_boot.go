package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/runtime/host/hostos"
)

const (
	grubDefaultFile     = "/etc/default/grub"
	kernelCmdlineFile   = "/etc/kernel/cmdline"
	initramfsModules    = "/etc/initramfs-tools/modules"
	dracutVFIOConf      = "/etc/dracut.conf.d/spinifex-vfio.conf"
	dracutForceDrivers  = "etc/cmdline.d/20-force_drivers.conf"
	modprobeDir         = "/etc/modprobe.d"
	vfioPCIConfName     = "vfio-pci.conf"
	nouveauConfName     = "blacklist-nouveau.conf"
	amdgpuConfName      = "blacklist-amdgpu.conf"
	initramfsVFIOModule = "vfio_pci"
)

// vfioEarlyModules load from the initramfs, before any GPU driver can claim the card.
var vfioEarlyModules = []string{"vfio", "vfio_iommu_type1", "vfio_pci"}

// gpuBoot changes the kernel command line and the initramfs with whichever
// tools the host uses. Paths are joined under root so tests run in a temp dir.
type gpuBoot struct {
	tools  hostos.BootTools
	root   string
	run    func(name string, args ...string) error
	output func(name string, args ...string) ([]byte, error)
}

func newGPUBoot() (*gpuBoot, error) {
	tools, err := hostos.SystemBootTools()
	if err != nil {
		return nil, err
	}
	if tools.Initramfs.Name == "dracut" {
		if _, err := exec.LookPath("lsinitrd"); err != nil {
			return nil, fmt.Errorf("lsinitrd, which checks the images dracut builds, is not on PATH: %w", err)
		}
	}
	return &gpuBoot{
		tools: tools,
		run: func(name string, args ...string) error {
			cmd := exec.Command(name, args...)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			return cmd.Run()
		},
		output: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).Output()
		},
	}, nil
}

func (b *gpuBoot) path(p string) string { return filepath.Join(b.root, p) }

// setKernelArgs adds the IOMMU parameters to every boot entry and to the
// command line that newly installed kernels get.
func (b *gpuBoot) setKernelArgs(params string) error {
	if b.tools.Bootloader.Name != "grubby" {
		changed, err := gpuSetupAddGRUBParams(b.path(grubDefaultFile), params)
		if err != nil {
			return err
		}
		if changed {
			fmt.Printf("    GRUB updated with: %s\n", params)
		} else {
			fmt.Println("    GRUB already has IOMMU params — refreshing")
		}
		if err := b.run(b.tools.Bootloader.Path); err != nil {
			return fmt.Errorf("%s failed: %w", b.tools.Bootloader.Name, err)
		}
		return nil
	}

	// --update-kernel=ALL also writes /etc/kernel/cmdline, which kernel-install
	// copies into the boot entry of every kernel installed later.
	if err := b.run(b.tools.Bootloader.Path, "--update-kernel=ALL", "--args="+params); err != nil {
		return fmt.Errorf("grubby --update-kernel=ALL failed: %w", err)
	}
	info, err := b.output(b.tools.Bootloader.Path, "--info=DEFAULT")
	if err != nil {
		return fmt.Errorf("grubby --info=DEFAULT failed: %w", err)
	}
	if missing := missingArgs(grubbyArgs(string(info)), params); len(missing) > 0 {
		return fmt.Errorf("the default boot entry lacks %s after grubby --update-kernel=ALL", strings.Join(missing, " "))
	}
	cmdline, err := os.ReadFile(b.path(kernelCmdlineFile))
	if err != nil {
		return fmt.Errorf("read %s, which new kernels take their arguments from: %w", kernelCmdlineFile, err)
	}
	if missing := missingArgs(string(cmdline), params); len(missing) > 0 {
		return fmt.Errorf("%s lacks %s, so newly installed kernels would boot without them", kernelCmdlineFile, strings.Join(missing, " "))
	}
	fmt.Printf("    Every boot entry and %s carry: %s\n", kernelCmdlineFile, params)
	return nil
}

// configureEarlyVFIO makes the initramfs load vfio-pci and carry its options
// and the driver blacklists. It reports whether it changed any file.
func (b *gpuBoot) configureEarlyVFIO(modprobeFiles []string) (bool, error) {
	if b.tools.Initramfs.Name == "dracut" {
		want := dracutVFIOConfig(modprobeFiles)
		if have, _ := os.ReadFile(b.path(dracutVFIOConf)); string(have) == want {
			fmt.Printf("    %s already configured\n", dracutVFIOConf)
			return false, nil
		}
		if err := os.MkdirAll(filepath.Dir(b.path(dracutVFIOConf)), 0o755); err != nil { //nolint:gosec // G301: dracut.conf.d is a conventional 0755 config directory.
			return false, err
		}
		if err := os.WriteFile(b.path(dracutVFIOConf), []byte(want), 0o644); err != nil { //nolint:gosec // G306: dracut config is conventionally world-readable (0644). Contains no secret material.
			return false, fmt.Errorf("write %s: %w", dracutVFIOConf, err)
		}
		fmt.Printf("    %s written\n", dracutVFIOConf)
		return true, nil
	}

	modData, _ := os.ReadFile(b.path(initramfsModules))
	if strings.Contains(string(modData), initramfsVFIOModule) {
		fmt.Println("    vfio modules already in initramfs")
		return false, nil
	}
	f, err := os.OpenFile(b.path(initramfsModules), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644) //nolint:gosec // G302: initramfs modules list is conventionally world-readable (0644). Contains no secret material.
	if err != nil {
		return false, fmt.Errorf("open initramfs modules: %w", err)
	}
	_, werr := fmt.Fprintf(f, "\n# vfio early binding for GPU passthrough\n%s\n", strings.Join(vfioEarlyModules, "\n"))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false, fmt.Errorf("write initramfs modules: %w", werr)
	}
	fmt.Println("    vfio modules added to initramfs")
	return true, nil
}

// dracutVFIOConfig uses force_drivers because add_drivers only copies a module
// into the image; force_drivers also loads it before udev probes devices.
func dracutVFIOConfig(modprobeFiles []string) string {
	var items []string
	for _, name := range modprobeFiles {
		items = append(items, filepath.Join(modprobeDir, name))
	}
	return "# Written by spx admin gpu setup: load vfio-pci before any GPU driver.\n" +
		"force_drivers+=\" " + strings.Join(vfioEarlyModules, " ") + " \"\n" +
		"install_items+=\" " + strings.Join(items, " ") + " \"\n"
}

// rebuildInitramfs regenerates the initramfs. A dracut rebuild is then checked
// image by image, since dracut can leave out a module or file without failing.
func (b *gpuBoot) rebuildInitramfs(modprobeFiles []string) error {
	if b.tools.Initramfs.Name != "dracut" {
		if err := b.run(b.tools.Initramfs.Path, "-u"); err != nil {
			return fmt.Errorf("%s failed: %w", b.tools.Initramfs.Name, err)
		}
		return nil
	}
	if err := b.run(b.tools.Initramfs.Path, "-f", "--regenerate-all"); err != nil {
		return fmt.Errorf("dracut -f --regenerate-all failed: %w", err)
	}
	kernels, err := b.installedKernels()
	if err != nil {
		return err
	}
	for _, kver := range kernels {
		if err := b.checkDracutImage(kver, modprobeFiles); err != nil {
			return err
		}
		fmt.Printf("    initramfs for %s loads vfio-pci early\n", kver)
	}
	return nil
}

// installedKernels lists the kernel versions dracut --regenerate-all builds for.
func (b *gpuBoot) installedKernels() ([]string, error) {
	entries, err := os.ReadDir(b.path("/lib/modules"))
	if err != nil {
		return nil, fmt.Errorf("list installed kernels: %w", err)
	}
	var kernels []string
	for _, e := range entries {
		if _, err := os.Stat(b.path(filepath.Join("/lib/modules", e.Name(), "modules.dep"))); err == nil {
			kernels = append(kernels, e.Name())
		}
	}
	if len(kernels) == 0 {
		return nil, fmt.Errorf("no installed kernel found under /lib/modules")
	}
	return kernels, nil
}

func (b *gpuBoot) checkDracutImage(kver string, modprobeFiles []string) error {
	listing, err := b.output("lsinitrd", "-k", kver)
	if err != nil {
		return fmt.Errorf("lsinitrd -k %s failed: %w", kver, err)
	}
	inImage := imageFiles(string(listing))
	builtin, _ := os.ReadFile(b.path(filepath.Join("/lib/modules", kver, "modules.builtin")))
	paths := strings.Fields(string(builtin))
	for f := range inImage {
		paths = append(paths, f)
	}
	modules := moduleNames(paths)

	var missing []string
	for _, mod := range vfioEarlyModules {
		if !modules[mod] {
			missing = append(missing, "module "+mod)
		}
	}
	for _, name := range modprobeFiles {
		if !inImage[filepath.Join("etc/modprobe.d", name)] {
			missing = append(missing, filepath.Join(modprobeDir, name))
		}
	}
	pre, err := b.output("lsinitrd", "-k", kver, "-f", dracutForceDrivers)
	if err != nil {
		return fmt.Errorf("lsinitrd -k %s -f %s failed: %w", kver, dracutForceDrivers, err)
	}
	if !slices.Contains(strings.Fields(string(pre)), "rd.driver.pre="+initramfsVFIOModule) {
		missing = append(missing, "rd.driver.pre="+initramfsVFIOModule)
	}
	if len(missing) > 0 {
		return fmt.Errorf("the initramfs for kernel %s lacks %s; check %s and rerun", kver, strings.Join(missing, ", "), dracutVFIOConf)
	}
	return nil
}

// imageFiles returns the paths in an lsinitrd listing, whose lines end in
// the path relative to the image root (symlinks add " -> target").
func imageFiles(listing string) map[string]bool {
	files := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(listing))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "l") {
			continue
		}
		line, _, _ = strings.Cut(line, " -> ")
		if fields := strings.Fields(line); len(fields) >= 9 {
			files[fields[len(fields)-1]] = true
		}
	}
	return files
}

var koSuffix = regexp.MustCompile(`\.ko(\.(xz|zst|gz))?$`)

// moduleNames maps module file paths to module names, which use underscores
// where file names may use dashes.
func moduleNames(paths []string) map[string]bool {
	names := map[string]bool{}
	for _, p := range paths {
		base := filepath.Base(p)
		if koSuffix.MatchString(base) {
			names[strings.ReplaceAll(koSuffix.ReplaceAllString(base, ""), "-", "_")] = true
		}
	}
	return names
}

// grubbyArgs returns the args= value from grubby --info output.
func grubbyArgs(info string) string {
	for line := range strings.SplitSeq(info, "\n") {
		if v, ok := strings.CutPrefix(line, "args="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func missingArgs(cmdline, params string) []string {
	have := strings.Fields(cmdline)
	var missing []string
	for p := range strings.FieldsSeq(params) {
		if !slices.Contains(have, p) {
			missing = append(missing, p)
		}
	}
	return missing
}

// gpuSetupAddGRUBParams appends iommu kernel params to GRUB_CMDLINE_LINUX_DEFAULT.
// Returns (true, nil) if the file was modified, (false, nil) if params were already present.
func gpuSetupAddGRUBParams(grubFile, params string) (bool, error) {
	data, err := os.ReadFile(grubFile)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", grubFile, err)
	}
	content := string(data)
	if strings.Contains(content, "intel_iommu=on") || strings.Contains(content, "amd_iommu=on") {
		return false, nil
	}
	re := regexp.MustCompile(`(GRUB_CMDLINE_LINUX_DEFAULT="[^"]*)"`)
	updated := re.ReplaceAllStringFunc(content, func(match string) string {
		groups := re.FindStringSubmatch(match)
		if len(groups) < 2 {
			return match
		}
		return groups[1] + " " + params + `"`
	})
	if updated == content {
		return false, fmt.Errorf("GRUB_CMDLINE_LINUX_DEFAULT not found in %s", grubFile)
	}
	if err := os.WriteFile(grubFile, []byte(updated), 0o644); err != nil { //nolint:gosec // G306: /etc/default/grub is read by grub-mkconfig and is conventionally 0644. Contains no secret material.
		return false, fmt.Errorf("write %s: %w", grubFile, err)
	}
	return true, nil
}
