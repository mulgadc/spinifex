package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mulgadc/spinifex/spinifex/admin"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/mulgadc/spinifex/spinifex/gpu"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var adminGpuCmd = &cobra.Command{
	Use:   "gpu",
	Short: "Manage GPU passthrough for a node",
}

var adminGpuStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show GPU hardware and passthrough state",
	Run:   runAdminGpuStatus,
}

var adminGpuEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable GPU passthrough on a node",
	Run:   runAdminGpuEnable,
}

var adminGpuDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Disable GPU passthrough on a node (blocked while GPU instances are running)",
	Run:   runAdminGpuDisable,
}

var adminGpuSetupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Configure this host for GPU passthrough (IOMMU, vfio-pci, nouveau blacklist)",
	Long: `Idempotent host setup for GPU passthrough. Run once before a reboot to configure
GRUB, vfio-pci early binding, and the nouveau blacklist; then run again after the
reboot to verify bindings and enable GPU passthrough in the daemon.

Must be run as root.`,
	Run: runAdminGpuSetup,
}

var adminGpuMigCmd = &cobra.Command{
	Use:   "mig",
	Short: "Manage NVIDIA MIG partitioning on this node",
}

var adminGpuMigStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show MIG capability, mode, and slice state for all GPUs on this node",
	Run:   runAdminGpuMigStatus,
}

var adminGpuMigEnableCmd = &cobra.Command{
	Use:   "enable",
	Short: "Enable MIG mode and create GPU instances with the given profile",
	Long: `Enable NVIDIA MIG mode on all MIG-capable GPUs (or a specific one) and
partition them into slices using the requested profile.

Must be run as root.`,
	Run: runAdminGpuMigEnable,
}

var adminGpuMigDisableCmd = &cobra.Command{
	Use:   "disable",
	Short: "Destroy all MIG instances and disable MIG mode",
	Long: `Destroy all GPU instances and disable NVIDIA MIG mode on all MIG-capable
GPUs (or a specific one). Blocked while GPU instances are running.

Must be run as root.`,
	Run: runAdminGpuMigDisable,
}

func init() {
	adminCmd.AddCommand(adminGpuCmd)
	adminGpuCmd.AddCommand(adminGpuStatusCmd)
	adminGpuCmd.AddCommand(adminGpuEnableCmd)
	adminGpuCmd.AddCommand(adminGpuDisableCmd)
	adminGpuCmd.AddCommand(adminGpuSetupCmd)
	adminGpuCmd.AddCommand(adminGpuMigCmd)
	adminGpuMigCmd.AddCommand(adminGpuMigStatusCmd)
	adminGpuMigCmd.AddCommand(adminGpuMigEnableCmd)
	adminGpuMigCmd.AddCommand(adminGpuMigDisableCmd)

	adminGpuStatusCmd.Flags().String("node", "", "Target node name (default: local node)")

	adminGpuMigEnableCmd.Flags().String("profile", "", "MIG profile to use (e.g. \"1g.10gb\") (required)")
	adminGpuMigEnableCmd.Flags().String("gpu", "", "PCI address of GPU to configure (default: all MIG-capable GPUs)")
	_ = adminGpuMigEnableCmd.MarkFlagRequired("profile")

	adminGpuMigDisableCmd.Flags().String("gpu", "", "PCI address of GPU to configure (default: all MIG-capable GPUs)")
	// enable, disable, setup, and mig subcommands write to the local spinifex.toml
	// and signal the local daemon — they must be run directly on the target host.
}

// gpuNodeStatus queries NATS and returns the NodeStatusResponse for the target node.
func gpuNodeStatus(targetNode string) (*types.NodeStatusResponse, error) {
	cfg, nc, err := loadConfigAndConnect()
	if err != nil {
		return nil, err
	}
	defer nc.Close()

	if targetNode == "" {
		targetNode = cfg.Node
	}

	responses, err := collectResponses(nc, "spinifex.node.status", 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("collect node status: %w", err)
	}
	for _, raw := range responses {
		var resp types.NodeStatusResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			continue
		}
		if resp.Node == targetNode {
			return &resp, nil
		}
	}
	return nil, fmt.Errorf("node %q not found or not responding", targetNode)
}

func runAdminGpuStatus(cmd *cobra.Command, _ []string) {
	targetNode, _ := cmd.Flags().GetString("node")
	resp, err := gpuNodeStatus(targetNode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	iommu := "unknown"
	vfio := "unknown"
	if resp.GPUCapable || len(resp.GPUModels) > 0 {
		iommu = "active"
		vfio = "loaded"
	}

	fmt.Printf("Node:            %s\n", resp.Node)
	if len(resp.GPUModels) > 0 {
		fmt.Printf("GPU hardware:    %s\n", strings.Join(resp.GPUModels, ", "))
	} else {
		fmt.Printf("GPU hardware:    none detected\n")
	}
	fmt.Printf("IOMMU:           %s\n", iommu)
	fmt.Printf("vfio-pci:        %s\n", vfio)

	if resp.GPUPassthrough {
		fmt.Printf("Passthrough:     enabled\n")
		fmt.Printf("GPU pool:        %d/%d allocated\n", resp.AllocGPUs, resp.TotalGPUs)

		var gpuTypes []string
		for _, cap := range resp.InstanceTypes {
			gpuTypes = append(gpuTypes, cap.Name)
		}
		if len(gpuTypes) > 0 {
			fmt.Printf("Instance types:  %s\n", strings.Join(gpuTypes, " "))
		}
	} else if resp.GPUCapable {
		fmt.Printf("Passthrough:     disabled\n")
		fmt.Printf("Instance types:  (none — run 'spx admin gpu enable' to activate)\n")
	} else {
		fmt.Printf("Passthrough:     disabled\n")
		fmt.Printf("Instance types:  (prerequisites not met)\n")
	}
}

// gpuToggle writes the TOML setting, sends SIGHUP, and polls until the daemon confirms the new state.
// enable/disable always operate on the local node; run the command directly on the target host.
func gpuToggle(_ *cobra.Command, enable bool) {
	cfgPath := viper.GetString("config")
	if cfgPath == "" {
		cfgPath = DefaultConfigFile()
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	localNode := cfg.Node

	// Check current state first.
	resp, err := gpuNodeStatus(localNode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if enable {
		if resp.GPUPassthrough {
			fmt.Println("GPU passthrough is already enabled.")
			return
		}
		if !resp.GPUCapable {
			fmt.Fprintln(os.Stderr, "Error: prerequisites not met on this node.")
			fmt.Fprintln(os.Stderr, "  Run 'sudo spx admin gpu setup' to configure the host.")
			os.Exit(1)
		}
	} else {
		if !resp.GPUPassthrough {
			fmt.Println("GPU passthrough is already disabled.")
			return
		}
		if resp.AllocGPUs > 0 {
			fmt.Fprintf(os.Stderr, "Error: %d GPU instance(s) are running. Terminate them first.\n", resp.AllocGPUs)
			os.Exit(1)
		}
	}

	// Write the TOML setting.
	if err := admin.SetGPUPassthrough(cfgPath, localNode, enable); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing config: %v\n", err)
		os.Exit(1)
	}

	// Signal the daemon to reload.
	out, err := exec.Command("systemctl", "kill", "-s", "HUP", "spinifex-daemon").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error sending SIGHUP: %v\n%s\n", err, out)
		os.Exit(1)
	}

	// Poll until the daemon confirms the new state.
	action := "enable"
	if !enable {
		action = "disable"
	}
	fmt.Printf("Waiting for daemon to %s GPU passthrough", action)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		fmt.Print(".")
		r, err := gpuNodeStatus(localNode)
		if err != nil {
			continue
		}
		if r.GPUPassthrough == enable {
			fmt.Println(" done.")
			// Refresh the MOTD banner immediately so the login prompt
			// reflects the new GPU passthrough state without requiring a reboot.
			_ = exec.Command("spx", "admin", "banner", "--boot-check").Run()
			runAdminGpuStatus(adminGpuStatusCmd, nil)
			return
		}
	}
	fmt.Println()
	fmt.Fprintln(os.Stderr, "Timed out waiting for daemon. Check: journalctl -u spinifex-daemon -n 50")
	os.Exit(1)
}

func runAdminGpuEnable(cmd *cobra.Command, _ []string) {
	gpuToggle(cmd, true)
}

func runAdminGpuDisable(cmd *cobra.Command, _ []string) {
	gpuToggle(cmd, false)
}

func runAdminGpuSetup(_ *cobra.Command, _ []string) {
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "Error: spx admin gpu setup must be run as root.")
		os.Exit(1)
	}

	fmt.Println("==> Detecting GPU")
	devices, err := gpu.Discover()
	if err != nil {
		fmt.Fprintf(os.Stderr, "GPU discovery failed: %v\n", err)
		os.Exit(1)
	}
	if len(devices) == 0 {
		fmt.Fprintln(os.Stderr, "No NVIDIA or AMD GPU found — is the card seated?")
		os.Exit(1)
	}
	for _, d := range devices {
		fmt.Printf("    %s - %s\n", d.PCIAddress, d.Model)
	}

	fmt.Println("==> Collecting PCI IDs for vfio-pci")
	ids := gpuSetupCollectVFIOIDs(devices)
	idsCSV := strings.Join(ids, ",")
	fmt.Printf("    IDs: %s\n", idsCSV)

	rebootNeeded := false

	fmt.Println("==> Checking IOMMU")
	iommuEntries, _ := os.ReadDir("/sys/kernel/iommu_groups/")
	if len(iommuEntries) > 0 {
		fmt.Println("    Active")
	} else {
		params := gpuSetupIOMMUParams()
		changed, err := gpuSetupAddGRUBParams(params)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to update GRUB: %v\n", err)
			os.Exit(1)
		}
		if changed {
			fmt.Printf("    GRUB updated with: %s\n", params)
		} else {
			fmt.Println("    GRUB already has IOMMU params — refreshing")
		}
		ug := exec.Command("update-grub")
		ug.Stdout, ug.Stderr = os.Stdout, os.Stderr
		if err := ug.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "update-grub failed: %v\n", err)
			os.Exit(1)
		}
		rebootNeeded = true
	}

	fmt.Println("==> Configuring vfio udev rule")
	const vfioUdevRule = "/etc/udev/rules.d/99-spinifex-vfio.rules"
	if _, err := os.Stat(vfioUdevRule); os.IsNotExist(err) {
		rule := "SUBSYSTEM==\"vfio\", GROUP=\"spinifex\", MODE=\"0660\"\n"
		if err := os.WriteFile(vfioUdevRule, []byte(rule), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write vfio udev rule: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("    Rule installed")
		_ = exec.Command("udevadm", "control", "--reload-rules").Run()
		_ = exec.Command("udevadm", "trigger", "--subsystem-match=vfio").Run()
	} else {
		fmt.Println("    Rule already present")
	}

	fmt.Println("==> Configuring nouveau blacklist")
	const nouveauConf = "/etc/modprobe.d/blacklist-nouveau.conf"
	if _, err := os.Stat(nouveauConf); os.IsNotExist(err) {
		if err := os.WriteFile(nouveauConf, []byte("blacklist nouveau\noptions nouveau modeset=0\n"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write nouveau blacklist: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("    nouveau blacklisted")
		rebootNeeded = true
	} else {
		fmt.Println("    Already blacklisted")
	}

	fmt.Println("==> Configuring amdgpu blacklist")
	const amdgpuConf = "/etc/modprobe.d/blacklist-amdgpu.conf"
	hasAMD := false
	for _, d := range devices {
		if d.Vendor == gpu.VendorAMD {
			hasAMD = true
			break
		}
	}
	if hasAMD {
		if _, err := os.Stat(amdgpuConf); os.IsNotExist(err) {
			if err := os.WriteFile(amdgpuConf, []byte("blacklist amdgpu\n"), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to write amdgpu blacklist: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("    amdgpu blacklisted")
			rebootNeeded = true
		} else {
			fmt.Println("    Already blacklisted")
		}
	} else {
		fmt.Println("    No AMD GPUs — skipping")
	}

	fmt.Println("==> Configuring vfio-pci early binding")
	const vfioPCIConf = "/etc/modprobe.d/vfio-pci.conf"
	existing, _ := os.ReadFile(vfioPCIConf)
	if !strings.Contains(string(existing), "ids="+idsCSV) {
		if err := os.WriteFile(vfioPCIConf, []byte("options vfio-pci ids="+idsCSV+"\n"), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write vfio-pci.conf: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("    vfio-pci.conf written (ids=%s)\n", idsCSV)
		rebootNeeded = true
	} else {
		fmt.Printf("    Already configured for %s\n", idsCSV)
	}

	const initramfsModules = "/etc/initramfs-tools/modules"
	modData, _ := os.ReadFile(initramfsModules)
	if !strings.Contains(string(modData), "vfio_pci") {
		f, err := os.OpenFile(initramfsModules, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to open initramfs modules: %v\n", err)
			os.Exit(1)
		}
		_, werr := fmt.Fprint(f, "\n# vfio early binding for GPU passthrough\nvfio\nvfio_iommu_type1\nvfio_pci\n")
		f.Close()
		if werr != nil {
			fmt.Fprintf(os.Stderr, "Failed to write initramfs modules: %v\n", werr)
			os.Exit(1)
		}
		fmt.Println("    vfio modules added to initramfs")
		rebootNeeded = true
	} else {
		fmt.Println("    vfio modules already in initramfs")
	}

	if rebootNeeded {
		fmt.Println("==> Updating initramfs")
		ui := exec.Command("update-initramfs", "-u")
		ui.Stdout, ui.Stderr = os.Stdout, os.Stderr
		if err := ui.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "update-initramfs failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("\nSetup complete — reboot required.")
		fmt.Println("  sudo reboot")
		fmt.Println("  Then run: sudo spx admin gpu enable")
		return
	}

	fmt.Println("==> Verifying vfio-pci module")
	if _, err := os.Stat("/sys/module/vfio_pci"); os.IsNotExist(err) {
		fmt.Fprintln(os.Stderr, "vfio_pci not loaded — check: journalctl -b | grep vfio")
		os.Exit(1)
	}
	fmt.Println("    Loaded")

	fmt.Println("==> Verifying GPU driver binding")
	for _, d := range devices {
		current, err := gpu.CurrentDriver(d.PCIAddress)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot read driver for %s: %v\n", d.PCIAddress, err)
			os.Exit(1)
		}
		if current == "vfio-pci" {
			fmt.Printf("    %s → vfio-pci\n", d.PCIAddress)
			continue
		}
		if current != "" {
			// Bound to native driver — blacklist/initramfs changes may not have taken effect.
			fmt.Fprintf(os.Stderr, "%s is bound to %q, not vfio-pci — reboot may be required\n", d.PCIAddress, current)
			fmt.Fprintf(os.Stderr, "  Check: ls -la /sys/bus/pci/devices/%s/driver\n", d.PCIAddress)
			os.Exit(1)
		}
		// Device is unbound (ids= did not auto-claim it) — bind explicitly via driver_override.
		fmt.Printf("    %s unbound — binding to vfio-pci\n", d.PCIAddress)
		if _, err := gpu.BindVFIO(d.PCIAddress); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to bind %s to vfio-pci: %v\n", d.PCIAddress, err)
			fmt.Fprintf(os.Stderr, "  Check: ls -la /sys/bus/pci/devices/%s/driver\n", d.PCIAddress)
			os.Exit(1)
		}
		fmt.Printf("    %s → vfio-pci\n", d.PCIAddress)
	}

	fmt.Println("==> Enabling GPU passthrough")
	gpuToggle(nil, true)
}

func runAdminGpuMigStatus(_ *cobra.Command, _ []string) {
	devices, err := gpu.Discover()
	if err != nil {
		fmt.Fprintf(os.Stderr, "GPU discovery failed: %v\n", err)
		os.Exit(1)
	}
	if len(devices) == 0 {
		fmt.Println("No GPUs detected on this node.")
		return
	}

	anyMIG := false
	for _, d := range devices {
		if d.MIGCapable {
			anyMIG = true
		}
		migMode := "N/A"
		if d.MIGCapable {
			if d.MIGEnabled {
				migMode = "enabled"
			} else {
				migMode = "disabled"
			}
		}
		fmt.Printf("GPU:           %s\n", d.PCIAddress)
		fmt.Printf("  Model:       %s\n", d.Model)
		fmt.Printf("  MIG capable: %v\n", d.MIGCapable)
		fmt.Printf("  MIG mode:    %s\n", migMode)

		if d.MIGEnabled {
			instances, listErr := gpu.ListInstances(d.PCIAddress)
			if listErr != nil {
				fmt.Printf("  Slices:      (error: %v)\n", listErr)
			} else {
				fmt.Printf("  Slices:      %d\n", len(instances))
				for _, inst := range instances {
					fmt.Printf("    GI %d: %-12s  mdev: %s\n",
						inst.GIID, inst.Profile.Name, inst.MdevPath)
				}
			}
		}
		fmt.Println()
	}

	if !anyMIG {
		fmt.Println("No MIG-capable GPUs found. MIG requires an NVIDIA A100, A30, or H100 GPU.")
	}
}

func runAdminGpuMigEnable(cmd *cobra.Command, _ []string) {
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "Error: spx admin gpu mig enable must be run as root.")
		os.Exit(1)
	}

	profileName, _ := cmd.Flags().GetString("profile")
	targetPCI, _ := cmd.Flags().GetString("gpu")

	cfgPath := viper.GetString("config")
	if cfgPath == "" {
		cfgPath = DefaultConfigFile()
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	resp, err := gpuNodeStatus(cfg.Node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp.AllocGPUs > 0 {
		fmt.Fprintf(os.Stderr, "Error: %d GPU instance(s) are running. Terminate them first.\n", resp.AllocGPUs)
		os.Exit(1)
	}

	devices, err := gpu.Discover()
	if err != nil {
		fmt.Fprintf(os.Stderr, "GPU discovery failed: %v\n", err)
		os.Exit(1)
	}

	targets := migFilterTargets(devices, targetPCI)
	if len(targets) == 0 {
		if targetPCI != "" {
			fmt.Fprintf(os.Stderr, "Error: no MIG-capable GPU found at PCI address %s\n", targetPCI)
		} else {
			fmt.Fprintln(os.Stderr, "Error: no MIG-capable GPUs found. MIG requires an NVIDIA A100, A30, or H100 GPU.")
		}
		os.Exit(1)
	}

	for _, d := range targets {
		fmt.Printf("==> Configuring MIG on %s (%s)\n", d.PCIAddress, d.Model)

		if !d.MIGEnabled {
			fmt.Println("    Enabling MIG mode...")
			if err := gpu.EnableMIGMode(d.PCIAddress); err != nil {
				fmt.Fprintf(os.Stderr, "    Error: %v\n", err)
				os.Exit(1)
			}
		} else {
			fmt.Println("    MIG mode already enabled")
		}

		profiles, err := gpu.ListProfiles(d.PCIAddress)
		if err != nil {
			fmt.Fprintf(os.Stderr, "    Error listing profiles: %v\n", err)
			os.Exit(1)
		}

		var selected *gpu.MIGProfile
		for i, p := range profiles {
			if p.Name == profileName {
				selected = &profiles[i]
				break
			}
		}
		if selected == nil {
			names := make([]string, len(profiles))
			for i, p := range profiles {
				names[i] = p.Name
			}
			fmt.Fprintf(os.Stderr, "    Error: profile %q not found. Available: %s\n",
				profileName, strings.Join(names, ", "))
			os.Exit(1)
		}

		existing, _ := gpu.ListInstances(d.PCIAddress)
		if len(existing) > 0 {
			fmt.Printf("    Destroying %d existing instance(s)...\n", len(existing))
			if err := gpu.DestroyAllInstances(d.PCIAddress); err != nil {
				fmt.Fprintf(os.Stderr, "    Error: %v\n", err)
				os.Exit(1)
			}
		}

		fmt.Printf("    Creating MIG instances (profile: %s)...\n", selected.Name)
		instances, err := gpu.CreateInstances(d.PCIAddress, *selected)
		if err != nil {
			fmt.Fprintf(os.Stderr, "    Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("    Created %d instance(s)\n", len(instances))
		for _, inst := range instances {
			fmt.Printf("      GI %d: %s  mdev: %s\n", inst.GIID, inst.Profile.Name, inst.MdevPath)
		}
	}

	fmt.Println("==> Writing mig_profile to config...")
	if err := admin.SetMIGProfile(cfgPath, cfg.Node, profileName); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing config: %v\n", err)
		os.Exit(1)
	}

	migReloadDaemon()
	fmt.Println("MIG enabled. Run 'spx admin gpu mig status' to verify.")
}

func runAdminGpuMigDisable(cmd *cobra.Command, _ []string) {
	if os.Getuid() != 0 {
		fmt.Fprintln(os.Stderr, "Error: spx admin gpu mig disable must be run as root.")
		os.Exit(1)
	}

	targetPCI, _ := cmd.Flags().GetString("gpu")

	cfgPath := viper.GetString("config")
	if cfgPath == "" {
		cfgPath = DefaultConfigFile()
	}
	cfg, err := config.LoadConfig(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}

	resp, err := gpuNodeStatus(cfg.Node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	if resp.AllocGPUs > 0 {
		fmt.Fprintf(os.Stderr, "Error: %d GPU instance(s) are running. Terminate them first.\n", resp.AllocGPUs)
		os.Exit(1)
	}

	devices, err := gpu.Discover()
	if err != nil {
		fmt.Fprintf(os.Stderr, "GPU discovery failed: %v\n", err)
		os.Exit(1)
	}

	targets := migFilterTargets(devices, targetPCI)
	if len(targets) == 0 {
		if targetPCI != "" {
			fmt.Fprintf(os.Stderr, "Error: no MIG-capable GPU found at PCI address %s\n", targetPCI)
		} else {
			fmt.Fprintln(os.Stderr, "Error: no MIG-capable GPUs found on this node.")
		}
		os.Exit(1)
	}

	for _, d := range targets {
		fmt.Printf("==> Disabling MIG on %s (%s)\n", d.PCIAddress, d.Model)
		if !d.MIGEnabled {
			fmt.Println("    MIG mode not active — skipping")
			continue
		}
		fmt.Println("    Destroying all GPU instances...")
		if err := gpu.DestroyAllInstances(d.PCIAddress); err != nil {
			fmt.Fprintf(os.Stderr, "    Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("    Disabling MIG mode...")
		if err := gpu.DisableMIGMode(d.PCIAddress); err != nil {
			fmt.Fprintf(os.Stderr, "    Error: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println("==> Clearing mig_profile from config...")
	if err := admin.SetMIGProfile(cfgPath, cfg.Node, ""); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing config: %v\n", err)
		os.Exit(1)
	}

	migReloadDaemon()
	fmt.Println("MIG disabled. GPUs will operate in whole-GPU passthrough mode.")
}

// migFilterTargets returns all MIG-capable GPUs, optionally filtered to a
// specific PCI address.
func migFilterTargets(devices []gpu.GPUDevice, targetPCI string) []gpu.GPUDevice {
	var out []gpu.GPUDevice
	for _, d := range devices {
		if !d.MIGCapable {
			continue
		}
		if targetPCI != "" && d.PCIAddress != targetPCI {
			continue
		}
		out = append(out, d)
	}
	return out
}

// migReloadDaemon sends SIGHUP to spinifex-daemon so it picks up the updated
// MIG configuration without a full restart.
func migReloadDaemon() {
	out, err := exec.Command("systemctl", "kill", "-s", "HUP", "spinifex-daemon").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to signal daemon: %v\n%s\n", err, out)
		fmt.Fprintln(os.Stderr, "Restart the daemon manually: sudo systemctl restart spinifex-daemon")
		return
	}
	fmt.Println("==> Daemon signalled (SIGHUP) — MIG pool will be rebuilt on next request.")
}

// gpuSetupCollectVFIOIDs collects vendor:device PCI ID pairs for each GPU and
// all sibling PCI functions on the same device slot (e.g. GPU + HDMI audio).
// All siblings must bind to vfio-pci together for IOMMU group isolation.
func gpuSetupCollectVFIOIDs(devices []gpu.GPUDevice) []string {
	seen := make(map[string]bool)
	var ids []string
	for _, d := range devices {
		dot := strings.LastIndex(d.PCIAddress, ".")
		if dot < 0 {
			continue
		}
		prefix := d.PCIAddress[:dot]
		matches, _ := filepath.Glob("/sys/bus/pci/devices/" + prefix + ".*")
		for _, m := range matches {
			vb, err1 := os.ReadFile(m + "/vendor")
			db, err2 := os.ReadFile(m + "/device")
			if err1 != nil || err2 != nil {
				continue
			}
			v := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(vb))), "0x")
			dv := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(string(db))), "0x")
			id := v + ":" + dv
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func gpuSetupIOMMUParams() string {
	data, _ := os.ReadFile("/proc/cpuinfo")
	if strings.Contains(string(data), "GenuineIntel") {
		return "intel_iommu=on iommu=pt"
	}
	return "amd_iommu=on iommu=pt"
}

// gpuSetupAddGRUBParams appends iommu kernel params to GRUB_CMDLINE_LINUX_DEFAULT.
// Returns (true, nil) if the file was modified, (false, nil) if params were already present.
func gpuSetupAddGRUBParams(params string) (bool, error) {
	const grubFile = "/etc/default/grub"
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
	if err := os.WriteFile(grubFile, []byte(updated), 0o644); err != nil {
		return false, fmt.Errorf("write %s: %w", grubFile, err)
	}
	return true, nil
}
