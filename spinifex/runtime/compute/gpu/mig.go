package gpu

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

var mdevBasePath = "/sys/bus/mdev/devices"

// ListProfiles returns the MIG profiles supported by the GPU at pciAddr.
// Requires nvidia-smi and MIG mode to be enabled on the device.
func ListProfiles(pciAddr string) ([]MIGProfile, error) {
	out, err := exec.Command("nvidia-smi", "mig", "-lgip", "-i", pciAddr).Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi mig -lgip: %w", err)
	}
	return parseMIGProfiles(string(out))
}

// EnableMIGMode enables MIG mode on the GPU at pciAddr. A reboot or driver
// reload may be required on some platforms before instances can be created.
func EnableMIGMode(pciAddr string) error {
	out, err := exec.Command("nvidia-smi", "-i", pciAddr, "-mig", "1").CombinedOutput()
	if err != nil {
		return fmt.Errorf("enable MIG mode on %s: %w\n%s", pciAddr, err, out)
	}
	slog.Info("MIG mode enabled", "gpu", pciAddr)
	return nil
}

// DisableMIGMode disables MIG mode on the GPU at pciAddr. All GPU instances
// must be destroyed before calling this.
func DisableMIGMode(pciAddr string) error {
	out, err := exec.Command("nvidia-smi", "-i", pciAddr, "-mig", "0").CombinedOutput()
	if err != nil {
		return fmt.Errorf("disable MIG mode on %s: %w\n%s", pciAddr, err, out)
	}
	slog.Info("MIG mode disabled", "gpu", pciAddr)
	return nil
}

// IsMIGModeEnabled reports whether MIG mode is currently active on pciAddr.
func IsMIGModeEnabled(pciAddr string) (bool, error) {
	out, err := exec.Command(
		"nvidia-smi",
		"--query-gpu=mig.mode.current",
		"--format=csv,noheader",
		"--id="+pciAddr,
	).Output()
	if err != nil {
		return false, fmt.Errorf("query MIG mode on %s: %w", pciAddr, err)
	}
	return strings.TrimSpace(string(out)) == "Enabled", nil
}

// CreateInstances creates the maximum number of GI+CI pairs on pciAddr using
// the given profile, then enumerates the resulting mdev UUIDs. It stops at the
// first nvidia-smi error rather than leaving a partially populated set.
func CreateInstances(pciAddr string, profile MIGProfile) ([]MIGInstance, error) {
	// Create as many GPU instances as the profile allows.
	giIDs, err := createGPUInstances(pciAddr, profile)
	if err != nil {
		// Attempt to clean up any GIs that were created before the failure.
		if destroyErr := DestroyAllInstances(pciAddr); destroyErr != nil {
			slog.Error("MIG cleanup after partial create failed", "gpu", pciAddr, "err", destroyErr)
		}
		return nil, err
	}

	// Create one compute instance per GPU instance.
	instances := make([]MIGInstance, 0, len(giIDs))
	for _, giID := range giIDs {
		ciID, err := createComputeInstance(pciAddr, giID)
		if err != nil {
			if destroyErr := DestroyAllInstances(pciAddr); destroyErr != nil {
				slog.Error("MIG cleanup after partial CI create failed", "gpu", pciAddr, "err", destroyErr)
			}
			return nil, fmt.Errorf("create compute instance in GI %d on %s: %w", giID, pciAddr, err)
		}
		instances = append(instances, MIGInstance{
			GIID:    giID,
			CIID:    ciID,
			Profile: profile,
		})
	}

	// Enumerate mdev UUIDs for the created instances.
	if err := enrichMdevPaths(mdevBasePath, pciAddr, instances); err != nil {
		return nil, fmt.Errorf("enumerate mdev paths on %s: %w", pciAddr, err)
	}

	slog.Info("MIG instances created", "gpu", pciAddr, "profile", profile.Name, "count", len(instances))
	return instances, nil
}

// ListInstances enumerates existing MIG GI+CI pairs on pciAddr and their mdev
// paths. Used to re-populate the GPU Manager pool on daemon restart.
func ListInstances(pciAddr string) ([]MIGInstance, error) {
	out, err := exec.Command("nvidia-smi", "mig", "-lgi", "-i", pciAddr).Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi mig -lgi on %s: %w", pciAddr, err)
	}
	instances, err := parseMIGInstances(string(out))
	if err != nil {
		return nil, err
	}
	if err := enrichMdevPaths(mdevBasePath, pciAddr, instances); err != nil {
		return nil, fmt.Errorf("enumerate mdev paths on %s: %w", pciAddr, err)
	}
	return instances, nil
}

// DestroyAllInstances destroys all GPU instances (and their compute instances)
// on pciAddr. This is required before disabling MIG mode.
func DestroyAllInstances(pciAddr string) error {
	out, err := exec.Command("nvidia-smi", "mig", "-dgi", "--gpu-instance-id=all", "-i", pciAddr).CombinedOutput()
	if err != nil {
		return fmt.Errorf("destroy MIG instances on %s: %w\n%s", pciAddr, err, out)
	}
	slog.Info("MIG instances destroyed", "gpu", pciAddr)
	return nil
}

// createGPUInstances creates GPU instances on pciAddr with the given profile,
// filling capacity, and returns their IDs.
func createGPUInstances(pciAddr string, profile MIGProfile) ([]int, error) {
	var giIDs []int
	for {
		out, err := exec.Command(
			"nvidia-smi", "mig", "-cgi", strconv.Itoa(profile.ID), "-i", pciAddr,
		).CombinedOutput()
		if err != nil {
			// nvidia-smi exits non-zero when capacity is exhausted; treat that as
			// the normal loop-termination signal rather than an error, provided we
			// created at least one instance.
			if len(giIDs) > 0 && isCapacityExhaustedError(string(out)) {
				break
			}
			return nil, fmt.Errorf("create GPU instance (profile %d) on %s: %w\n%s", profile.ID, pciAddr, err, out)
		}
		id, err := parseCreatedGIID(string(out))
		if err != nil {
			return nil, err
		}
		giIDs = append(giIDs, id)
	}
	return giIDs, nil
}

// createComputeInstance creates the default compute instance within giID and
// returns its compute instance ID.
func createComputeInstance(pciAddr string, giID int) (int, error) {
	out, err := exec.Command(
		"nvidia-smi", "mig", "-cci",
		"-gi", strconv.Itoa(giID),
		"-i", pciAddr,
	).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("nvidia-smi mig -cci: %w\n%s", err, out)
	}
	return parseCreatedCIID(string(out))
}

// enrichMdevPaths matches mdev UUIDs to instances via symlink target (parent GPU
// PCI address) and gpu_instance_id sysfs attribute. basePath is parameterised
// so tests can inject a temp dir without touching real sysfs.
func enrichMdevPaths(basePath, pciAddr string, instances []MIGInstance) error {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("mdev subsystem not present at %s — is the NVIDIA driver loaded?", basePath)
		}
		return fmt.Errorf("read mdev devices: %w", err)
	}

	// Build a map from GIID → *MIGInstance for fast matching.
	byGI := make(map[int]*MIGInstance, len(instances))
	for i := range instances {
		byGI[instances[i].GIID] = &instances[i]
	}

	for _, entry := range entries {
		uuid := entry.Name()
		// Each entry under /sys/bus/mdev/devices is a symlink whose target path
		// includes the parent GPU's PCI address, e.g.:
		//   ../../../../devices/pci0000:00/0000:00:03.1/0000:01:00.0/<uuid>
		// Reading the symlink directly is the only portable way to identify the
		// parent GPU — the mdev directory itself exposes no stable "parent" file.
		target, err := os.Readlink(filepath.Join(basePath, uuid))
		if err != nil || !strings.Contains(target, pciAddr) {
			continue
		}

		mdevPath := filepath.Join(basePath, uuid)
		giID, err := readMdevGIID(mdevPath)
		if err != nil {
			slog.Debug("mig: cannot read GI ID from mdev", "uuid", uuid, "err", err)
			continue
		}

		if inst, ok := byGI[giID]; ok {
			inst.UUID = uuid
			inst.MdevPath = mdevPath
		}
	}

	// Verify all instances got a path.
	for _, inst := range instances {
		if inst.MdevPath == "" {
			return fmt.Errorf("no mdev path found for MIG GI %d on %s", inst.GIID, pciAddr)
		}
	}
	return nil
}

// readMdevGIID reads the GPU instance ID from a mdev device's sysfs attributes.
func readMdevGIID(mdevPath string) (int, error) {
	raw, err := readSysfsString(filepath.Join(mdevPath, "gpu_instance_id"))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(raw))
}

// parseMIGProfiles parses the text output of `nvidia-smi mig -lgip`.
// Example lines (after header):
//
//	GPU  0  MIG 1g.10gb  Profile  ID: 9   ...
func parseMIGProfiles(output string) ([]MIGProfile, error) {
	var profiles []MIGProfile
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		// Look for lines that contain a profile name like "1g.10gb" and an ID.
		if !strings.Contains(line, "MIG") || !strings.Contains(line, "Profile") {
			continue
		}
		name, id, memMiB, ok := extractProfileFields(line)
		if !ok {
			continue
		}
		profiles = append(profiles, MIGProfile{ID: id, Name: name, MemoryMiB: memMiB})
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("no MIG profiles found in nvidia-smi output")
	}
	return profiles, nil
}

// parseMIGInstances parses the text output of `nvidia-smi mig -lgi`.
func parseMIGInstances(output string) ([]MIGInstance, error) {
	var instances []MIGInstance
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "MIG") {
			continue
		}
		giID, profileName, memMiB, ok := extractGIFields(line)
		if !ok {
			continue
		}
		instances = append(instances, MIGInstance{
			GIID: giID,
			Profile: MIGProfile{
				Name:      profileName,
				MemoryMiB: memMiB,
			},
		})
	}
	return instances, nil
}

// extractProfileFields pulls name, ID, and memory from a `mig -lgip` line.
// Returns ok=false if the line doesn't match the expected format.
func extractProfileFields(line string) (name string, id int, memMiB int64, ok bool) {
	// Expected format contains tokens like: "MIG 1g.10gb" and "ID: 9"
	parts := strings.Fields(line)
	for i, p := range parts {
		if strings.Contains(p, "g.") && !strings.HasPrefix(p, "GPU") {
			name = p
		}
		if p == "ID:" && i+1 < len(parts) {
			idStr := strings.TrimRight(parts[i+1], ",")
			v, err := strconv.Atoi(idStr)
			if err == nil {
				id = v
				ok = true
			}
		}
	}
	if name == "" || !ok {
		return "", 0, 0, false
	}
	memMiB = parseMIGMemory(name)
	return name, id, memMiB, true
}

// extractGIFields pulls GI ID, profile name, and memory from a `mig -lgi` line.
func extractGIFields(line string) (giID int, profileName string, memMiB int64, ok bool) {
	parts := strings.Fields(line)
	for i, p := range parts {
		if strings.Contains(p, "g.") {
			profileName = p
		}
		// GI ID appears as a bare integer early in the line; heuristic: first
		// token after the GPU index column that is purely numeric.
		if i > 0 && i < 4 {
			if v, err := strconv.Atoi(p); err == nil {
				giID = v
				ok = true
			}
		}
	}
	if !ok || profileName == "" {
		return 0, "", 0, false
	}
	memMiB = parseMIGMemory(profileName)
	return giID, profileName, memMiB, true
}

// parseMIGMemory extracts the MiB value encoded in a profile name like "3g.40gb"
// or Blackwell-style variants like "1g.24gb+gfx", "1g.24gb-me", "1g.24gb+me.all".
func parseMIGMemory(name string) int64 {
	parts := strings.SplitN(name, ".", 2)
	if len(parts) != 2 {
		return 0
	}
	memPart := parts[1]
	// Strip Blackwell capability suffixes (+gfx, -me, +me, +me.all, etc.)
	if idx := strings.IndexAny(memPart, "+-"); idx >= 0 {
		memPart = memPart[:idx]
	}
	gbStr := strings.TrimSuffix(memPart, "gb")
	gb, err := strconv.ParseInt(gbStr, 10, 64)
	if err != nil {
		return 0
	}
	return gb * 1024
}

// parseCreatedGIID extracts the newly created GPU instance ID from nvidia-smi output.
// Example: "Successfully created GPU instance ID  2 on GPU  0 ...".
func parseCreatedGIID(output string) (int, error) {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "Successfully created GPU instance") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "ID" && i+1 < len(parts) {
					return strconv.Atoi(parts[i+1])
				}
			}
		}
	}
	return 0, fmt.Errorf("could not parse GPU instance ID from: %q", output)
}

// parseCreatedCIID extracts the newly created compute instance ID from nvidia-smi output.
// Example: "Successfully created compute instance ID  0 on GPU  0 GPU instance ID  2 ...".
func parseCreatedCIID(output string) (int, error) {
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "Successfully created compute instance") {
			parts := strings.Fields(line)
			for i, p := range parts {
				if p == "ID" && i+1 < len(parts) {
					if v, err := strconv.Atoi(parts[i+1]); err == nil {
						return v, nil
					}
				}
			}
		}
	}
	return 0, fmt.Errorf("could not parse compute instance ID from: %q", output)
}

// isCapacityExhaustedError reports whether nvidia-smi output indicates that the
// GPU has no remaining capacity for additional instances of the requested profile.
func isCapacityExhaustedError(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "no space left") ||
		strings.Contains(lower, "insufficient resources") ||
		strings.Contains(lower, "invalid argument")
}
