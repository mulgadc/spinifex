package gpu

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixture output from `nvidia-smi mig -lgip` on an A100 80GB.
// Each data line contains both "MIG" and "Profile" with an "ID: <n>" token.
const lgipOutputA100 = `
+-------------------------------------------------------------------------------------------+
| GPU instance profiles:                                                                     |
| GPU   Name             Profile  ID    Instances   Memory     P2P    SM    CE    DEC   ENC |
|===========================================================================================|
|   0  MIG 1g.10gb  Profile  ID: 19,  Instances: 7/7   Mem: 9.62 GiB                      |
|   0  MIG 2g.20gb  Profile  ID: 9,   Instances: 3/3   Mem: 19.62 GiB                     |
|   0  MIG 3g.40gb  Profile  ID: 5,   Instances: 2/2   Mem: 39.25 GiB                     |
|   0  MIG 7g.80gb  Profile  ID: 0,   Instances: 1/1   Mem: 79.25 GiB                     |
+-------------------------------------------------------------------------------------------+
`

// Fixture output from `nvidia-smi mig -lgi` on an A100 80GB with two 1g.10gb slices.
// Data lines must contain "MIG", with the GI ID as the first bare integer in positions 1–3.
const lgiOutputA100 = `
+--------------------------------------------------------------------+
| Existing GPU Instances on GPU 0                                    |
|--------------------------------------------------------------------|
| GPU  GI  Profile  Placement   ...                                  |
|====================================================================|
|   0   1  MIG 1g.10gb    0:1   P2P: No                            |
|   0   2  MIG 1g.10gb    1:1   P2P: No                            |
+--------------------------------------------------------------------+
`

// Fixture output from `nvidia-smi mig -lgip` on an RTX Pro 6000 Blackwell SE.
// Blackwell uses 4-way partitioning (1/4, 1/2, full) with optional +gfx/-me suffixes.
const lgipOutputRTX6000SE = `
+-------------------------------------------------------------------------------------------+
| GPU instance profiles:                                                                     |
| GPU   Name                   Profile  ID    Instances   Memory     SM   ...               |
|===========================================================================================|
|   0  MIG 1g.24gb             Profile  ID: 14,  Instances: 4/4   Mem: 24.00 GiB           |
|   0  MIG 1g.24gb+me          Profile  ID: 15,  Instances: 1/4   Mem: 24.00 GiB           |
|   0  MIG 1g.24gb+gfx         Profile  ID: 16,  Instances: 4/4   Mem: 24.00 GiB           |
|   0  MIG 2g.48gb             Profile  ID: 7,   Instances: 2/2   Mem: 48.00 GiB           |
|   0  MIG 2g.48gb+gfx         Profile  ID: 8,   Instances: 2/2   Mem: 48.00 GiB           |
|   0  MIG 4g.96gb             Profile  ID: 2,   Instances: 1/1   Mem: 96.00 GiB           |
+-------------------------------------------------------------------------------------------+
`

// Fixture output from `nvidia-smi mig -lgi` on an RTX Pro 6000 SE with four 1g.24gb slices.
const lgiOutputRTX6000SE = `
+--------------------------------------------------------------------+
| Existing GPU Instances on GPU 0                                    |
|--------------------------------------------------------------------|
|   0   1  MIG 1g.24gb    0:1   P2P: No                            |
|   0   2  MIG 1g.24gb    1:1   P2P: No                            |
|   0   3  MIG 1g.24gb    2:1   P2P: No                            |
|   0   4  MIG 1g.24gb    3:1   P2P: No                            |
+--------------------------------------------------------------------+
`

// Fixture output from `nvidia-smi mig -lgi` on an RTX Pro 6000 SE with +gfx instances.
const lgiOutputRTX6000SEGfx = `
+--------------------------------------------------------------------+
| Existing GPU Instances on GPU 0                                    |
|--------------------------------------------------------------------|
|   0   1  MIG 1g.24gb+gfx    0:1   P2P: No                        |
|   0   2  MIG 1g.24gb+gfx    1:1   P2P: No                        |
+--------------------------------------------------------------------+
`

// Fixture output from `nvidia-smi mig -lgi` on an RTX Pro 6000 SE with a 2g.48gb slice.
const lgiOutputRTX6000SE2g = `
+--------------------------------------------------------------------+
| Existing GPU Instances on GPU 0                                    |
|--------------------------------------------------------------------|
|   0   1  MIG 2g.48gb    0:2   P2P: No                            |
+--------------------------------------------------------------------+
`

// Fixture output from `nvidia-smi mig -lgi` on an H100 with mixed profiles.
const lgiOutputMixedProfiles = `
+--------------------------------------------------------------------+
| Existing GPU Instances on GPU 0                                    |
|====================================================================|
|   0   1  MIG 3g.40gb    0:4   P2P: No                            |
|   0   5  MIG 1g.10gb    4:1   P2P: No                            |
+--------------------------------------------------------------------+
`

// --- parseMIGMemory ---

func TestParseMIGMemory(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantMiB int64
	}{
		{"1g.10gb", "1g.10gb", 10 * 1024},
		{"2g.20gb", "2g.20gb", 20 * 1024},
		{"3g.40gb", "3g.40gb", 40 * 1024},
		{"4g.40gb", "4g.40gb", 40 * 1024},
		{"7g.80gb", "7g.80gb", 80 * 1024},
		// Blackwell suffix variants
		{"1g.24gb", "1g.24gb", 24 * 1024},
		{"2g.48gb", "2g.48gb", 48 * 1024},
		{"4g.96gb", "4g.96gb", 96 * 1024},
		{"1g.24gb+gfx", "1g.24gb+gfx", 24 * 1024},
		{"1g.24gb-me", "1g.24gb-me", 24 * 1024},
		{"1g.24gb+me", "1g.24gb+me", 24 * 1024},
		{"1g.24gb+me.all", "1g.24gb+me.all", 24 * 1024},
		{"2g.48gb+gfx", "2g.48gb+gfx", 48 * 1024},
		{"empty", "", 0},
		{"no dot", "1g10gb", 0},
		{"non-numeric gb", "1g.xgb", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantMiB, parseMIGMemory(tt.input))
		})
	}
}

// --- isCapacityExhaustedError ---

func TestIsCapacityExhaustedError(t *testing.T) {
	exhausted := []string{
		"No space left on device",
		"Insufficient resources to create GPU instance",
		"Invalid argument",
		"ERROR: no space left for another MIG instance",
		"insufficient resources",
	}
	for _, msg := range exhausted {
		assert.True(t, isCapacityExhaustedError(msg), "expected exhausted for: %q", msg)
	}

	notExhausted := []string{
		"",
		"Successfully created GPU instance ID 1 on GPU 0",
		"unknown error",
		"permission denied",
	}
	for _, msg := range notExhausted {
		assert.False(t, isCapacityExhaustedError(msg), "expected not-exhausted for: %q", msg)
	}
}

// --- extractProfileFields ---

func TestExtractProfileFields(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantName  string
		wantID    int
		wantMemMB int64
		wantOK    bool
	}{
		{
			name:      "1g.10gb profile ID 19",
			line:      "  0  MIG 1g.10gb  Profile  ID: 19,  Instances: 7/7",
			wantName:  "1g.10gb",
			wantID:    19,
			wantMemMB: 10 * 1024,
			wantOK:    true,
		},
		{
			name:      "7g.80gb profile ID 0",
			line:      "  0  MIG 7g.80gb  Profile  ID: 0,   Instances: 1/1",
			wantName:  "7g.80gb",
			wantID:    0,
			wantMemMB: 80 * 1024,
			wantOK:    true,
		},
		{
			name:      "ID with no trailing comma",
			line:      "  0  MIG 3g.40gb  Profile  ID: 5",
			wantName:  "3g.40gb",
			wantID:    5,
			wantMemMB: 40 * 1024,
			wantOK:    true,
		},
		{
			name:      "Blackwell 1g.24gb+gfx profile",
			line:      "  0  MIG 1g.24gb+gfx  Profile  ID: 16,  Instances: 4/4",
			wantName:  "1g.24gb+gfx",
			wantID:    16,
			wantMemMB: 24 * 1024,
			wantOK:    true,
		},
		{
			name:      "Blackwell 2g.48gb profile",
			line:      "  0  MIG 2g.48gb  Profile  ID: 7,  Instances: 2/2",
			wantName:  "2g.48gb",
			wantID:    7,
			wantMemMB: 48 * 1024,
			wantOK:    true,
		},
		{
			name:      "Blackwell 4g.96gb profile",
			line:      "  0  MIG 4g.96gb  Profile  ID: 2,  Instances: 1/1",
			wantName:  "4g.96gb",
			wantID:    2,
			wantMemMB: 96 * 1024,
			wantOK:    true,
		},
		{
			name:   "no profile name token",
			line:   "  0  MIG Profile  ID: 9",
			wantOK: false,
		},
		{
			name:   "no ID token",
			line:   "  0  MIG 1g.10gb  Profile  Instances: 7/7",
			wantOK: false,
		},
		{
			name:   "header line without profile name",
			line:   "GPU   Name   Profile  ID    Instances",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, id, memMiB, ok := extractProfileFields(tt.line)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantName, name)
				assert.Equal(t, tt.wantID, id)
				assert.Equal(t, tt.wantMemMB, memMiB)
			}
		})
	}
}

// --- extractGIFields ---

func TestExtractGIFields(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantGIID    int
		wantProfile string
		wantMemMiB  int64
		wantOK      bool
	}{
		{
			name:        "GI 1 profile 1g.10gb",
			line:        "  0   1  MIG 1g.10gb    0:1",
			wantGIID:    1,
			wantProfile: "1g.10gb",
			wantMemMiB:  10 * 1024,
			wantOK:      true,
		},
		{
			name:        "GI 5 profile 1g.10gb",
			line:        "  0   5  MIG 1g.10gb    4:1",
			wantGIID:    5,
			wantProfile: "1g.10gb",
			wantMemMiB:  10 * 1024,
			wantOK:      true,
		},
		{
			name:        "GI 1 profile 3g.40gb",
			line:        "  0   1  MIG 3g.40gb    0:4",
			wantGIID:    1,
			wantProfile: "3g.40gb",
			wantMemMiB:  40 * 1024,
			wantOK:      true,
		},
		{
			name:        "Blackwell GI 1 profile 1g.24gb",
			line:        "  0   1  MIG 1g.24gb    0:1",
			wantGIID:    1,
			wantProfile: "1g.24gb",
			wantMemMiB:  24 * 1024,
			wantOK:      true,
		},
		{
			name:        "Blackwell GI 1 profile 1g.24gb+gfx",
			line:        "  0   1  MIG 1g.24gb+gfx    0:1",
			wantGIID:    1,
			wantProfile: "1g.24gb+gfx",
			wantMemMiB:  24 * 1024,
			wantOK:      true,
		},
		{
			name:        "Blackwell GI 1 profile 2g.48gb",
			line:        "  0   1  MIG 2g.48gb    0:2",
			wantGIID:    1,
			wantProfile: "2g.48gb",
			wantMemMiB:  48 * 1024,
			wantOK:      true,
		},
		{
			name:   "no MIG profile token",
			line:   "  0   1  NoProfile",
			wantOK: false,
		},
		{
			name:   "only header text",
			line:   "GPU  GI  Profile  Placement",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			giID, profile, memMiB, ok := extractGIFields(tt.line)
			assert.Equal(t, tt.wantOK, ok)
			if tt.wantOK {
				assert.Equal(t, tt.wantGIID, giID)
				assert.Equal(t, tt.wantProfile, profile)
				assert.Equal(t, tt.wantMemMiB, memMiB)
			}
		})
	}
}

// --- parseMIGProfiles ---

func TestParseMIGProfiles_A100(t *testing.T) {
	profiles, err := parseMIGProfiles(lgipOutputA100)
	require.NoError(t, err)
	require.Len(t, profiles, 4)

	assert.Equal(t, "1g.10gb", profiles[0].Name)
	assert.Equal(t, 19, profiles[0].ID)
	assert.Equal(t, int64(10*1024), profiles[0].MemoryMiB)

	assert.Equal(t, "2g.20gb", profiles[1].Name)
	assert.Equal(t, 9, profiles[1].ID)

	assert.Equal(t, "7g.80gb", profiles[3].Name)
	assert.Equal(t, 0, profiles[3].ID)
	assert.Equal(t, int64(80*1024), profiles[3].MemoryMiB)
}

func TestParseMIGProfiles_RTX6000SE(t *testing.T) {
	profiles, err := parseMIGProfiles(lgipOutputRTX6000SE)
	require.NoError(t, err)
	require.Len(t, profiles, 6)

	assert.Equal(t, "1g.24gb", profiles[0].Name)
	assert.Equal(t, 14, profiles[0].ID)
	assert.Equal(t, int64(24*1024), profiles[0].MemoryMiB)

	assert.Equal(t, "1g.24gb+gfx", profiles[2].Name)
	assert.Equal(t, 16, profiles[2].ID)
	assert.Equal(t, int64(24*1024), profiles[2].MemoryMiB)

	assert.Equal(t, "4g.96gb", profiles[5].Name)
	assert.Equal(t, 2, profiles[5].ID)
	assert.Equal(t, int64(96*1024), profiles[5].MemoryMiB)
}

func TestParseMIGInstances_RTX6000SE_FourSlices(t *testing.T) {
	instances, err := parseMIGInstances(lgiOutputRTX6000SE)
	require.NoError(t, err)
	require.Len(t, instances, 4)

	for i, inst := range instances {
		assert.Equal(t, i+1, inst.GIID)
		assert.Equal(t, "1g.24gb", inst.Profile.Name)
		assert.Equal(t, int64(24*1024), inst.Profile.MemoryMiB)
	}
}

func TestParseMIGInstances_RTX6000SE_GfxSlices(t *testing.T) {
	instances, err := parseMIGInstances(lgiOutputRTX6000SEGfx)
	require.NoError(t, err)
	require.Len(t, instances, 2)

	assert.Equal(t, "1g.24gb+gfx", instances[0].Profile.Name)
	assert.Equal(t, int64(24*1024), instances[0].Profile.MemoryMiB)
}

func TestParseMIGInstances_RTX6000SE_2gSlice(t *testing.T) {
	instances, err := parseMIGInstances(lgiOutputRTX6000SE2g)
	require.NoError(t, err)
	require.Len(t, instances, 1)

	assert.Equal(t, 1, instances[0].GIID)
	assert.Equal(t, "2g.48gb", instances[0].Profile.Name)
	assert.Equal(t, int64(48*1024), instances[0].Profile.MemoryMiB)
}

func TestParseMIGProfiles_Empty(t *testing.T) {
	_, err := parseMIGProfiles("")
	assert.Error(t, err)
}

func TestParseMIGProfiles_NoMatchingLines(t *testing.T) {
	_, err := parseMIGProfiles("header only\nno data here\n")
	assert.Error(t, err)
}

// --- parseMIGInstances ---

func TestParseMIGInstances_TwoSlices(t *testing.T) {
	instances, err := parseMIGInstances(lgiOutputA100)
	require.NoError(t, err)
	require.Len(t, instances, 2)

	assert.Equal(t, 1, instances[0].GIID)
	assert.Equal(t, "1g.10gb", instances[0].Profile.Name)
	assert.Equal(t, int64(10*1024), instances[0].Profile.MemoryMiB)

	assert.Equal(t, 2, instances[1].GIID)
	assert.Equal(t, "1g.10gb", instances[1].Profile.Name)
}

func TestParseMIGInstances_MixedProfiles(t *testing.T) {
	instances, err := parseMIGInstances(lgiOutputMixedProfiles)
	require.NoError(t, err)
	require.Len(t, instances, 2)

	assert.Equal(t, 1, instances[0].GIID)
	assert.Equal(t, "3g.40gb", instances[0].Profile.Name)
	assert.Equal(t, int64(40*1024), instances[0].Profile.MemoryMiB)

	assert.Equal(t, 5, instances[1].GIID)
	assert.Equal(t, "1g.10gb", instances[1].Profile.Name)
}

func TestParseMIGInstances_Empty(t *testing.T) {
	instances, err := parseMIGInstances("")
	require.NoError(t, err)
	assert.Empty(t, instances)
}

// --- parseCreatedGIID ---

func TestParseCreatedGIID(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantID  int
		wantErr bool
	}{
		{
			name:   "single line success",
			output: "Successfully created GPU instance ID  2 on GPU  0 using profile MIG 1g.10gb (ID 19)",
			wantID: 2,
		},
		{
			name:   "ID zero",
			output: "Successfully created GPU instance ID  0 on GPU  0 using profile MIG 7g.80gb (ID 0)",
			wantID: 0,
		},
		{
			name:   "multi-line output with success line",
			output: "Warning: something\nSuccessfully created GPU instance ID  5 on GPU  0 ...\nDone.",
			wantID: 5,
		},
		{
			name:    "no success line",
			output:  "ERROR: failed to create GPU instance",
			wantErr: true,
		},
		{
			name:    "empty output",
			output:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := parseCreatedGIID(tt.output)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantID, id)
			}
		})
	}
}

// --- parseCreatedCIID ---

func TestParseCreatedCIID(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		wantID  int
		wantErr bool
	}{
		{
			name:   "standard output",
			output: "Successfully created compute instance ID  0 on GPU  0 GPU instance ID  2 using profile MIG 1g.10gb/1 (ID 0)",
			wantID: 0,
		},
		{
			name:   "non-zero CI ID",
			output: "Successfully created compute instance ID  3 on GPU  0 GPU instance ID  1",
			wantID: 3,
		},
		{
			name:    "no success line",
			output:  "ERROR: could not create compute instance",
			wantErr: true,
		},
		{
			name:    "empty",
			output:  "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := parseCreatedCIID(tt.output)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantID, id)
			}
		})
	}
}

// --- enrichMdevPaths ---

func TestEnrichMdevPaths_MatchesBySymlinkTarget(t *testing.T) {
	// Simulate /sys/bus/mdev/devices with two symlinks whose targets contain a
	// PCI address. Only the one matching our target GPU should be matched.
	root := t.TempDir()
	pciAddr := "0000:01:00.0"
	otherPCI := "0000:02:00.0"

	// Fake mdev device directories — one per GPU.
	devDir1 := t.TempDir()
	devDir2 := t.TempDir()

	uuid1 := "aaaaaaaa-0000-0000-0000-000000000001"
	uuid2 := "bbbbbbbb-0000-0000-0000-000000000002"

	// Write gpu_instance_id files so readMdevGIID succeeds.
	require.NoError(t, os.WriteFile(filepath.Join(devDir1, "gpu_instance_id"), []byte("1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(devDir2, "gpu_instance_id"), []byte("3\n"), 0o644))

	// Symlink targets embed the PCI address the way the real NVIDIA driver does.
	target1 := filepath.Join("../../../../devices/pci0000:00/0000:00:03.1", pciAddr, uuid1)
	target2 := filepath.Join("../../../../devices/pci0000:00/0000:00:04.0", otherPCI, uuid2)

	require.NoError(t, os.Symlink(devDir1, filepath.Join(root, uuid1)))
	require.NoError(t, os.Symlink(devDir2, filepath.Join(root, uuid2)))

	// But os.Readlink reads the raw target string, not the resolved path. Create
	// a helper directory whose name contains the PCI address so Readlink returns a
	// string that contains pciAddr.
	_ = target1
	_ = target2

	// Re-create the symlinks with targets that contain the PCI address as a string
	// component (simulating the real sysfs layout).
	require.NoError(t, os.Remove(filepath.Join(root, uuid1)))
	require.NoError(t, os.Remove(filepath.Join(root, uuid2)))

	fakeTarget1 := "../../../../devices/pci0000:00/0000:00:03.1/" + pciAddr + "/" + uuid1
	fakeTarget2 := "../../../../devices/pci0000:00/0000:00:04.0/" + otherPCI + "/" + uuid2
	require.NoError(t, os.Symlink(fakeTarget1, filepath.Join(root, uuid1)))
	require.NoError(t, os.Symlink(fakeTarget2, filepath.Join(root, uuid2)))

	// Also create the actual device directories so readMdevGIID can read from them.
	// Since the symlink targets are relative and don't resolve in our temp dir,
	// we recreate the symlinks pointing directly at devDir1/devDir2 but with
	// a target string containing the PCI address.
	// Simplest: just point to absolute paths that contain pciAddr in the string.
	// We can't do that cleanly with os.Symlink + absolute paths, so test the
	// parent-matching logic in isolation instead: re-point to absolute devDirs
	// but patch the symlink target string to include pciAddr.
	//
	// The only part enrichMdevPaths uses from the symlink is
	// strings.Contains(target, pciAddr). We need the target string to contain it;
	// the path doesn't need to resolve. But we DO need to read gpu_instance_id, so
	// the mdev path must resolve to devDir. Use a two-level layout: the symlink
	// target embeds pciAddr in its string even though Go will not resolve through
	// relative components in this temp dir.
	//
	// Easiest correct approach: the symlinks already point to the fakeTargets which
	// contain the PCI address. For reading gpu_instance_id, use the resolved mdev
	// path = filepath.Join(root, uuid) which Go resolves through the symlink.
	// Since fakeTarget1 is relative and doesn't resolve within root, create the
	// gpu_instance_id inside root/<uuid>/... instead. Just put the attribute files
	// directly in the symlink source path isn't possible for broken symlinks.
	//
	// Use absolute symlinks that contain pciAddr in the path string.
	require.NoError(t, os.Remove(filepath.Join(root, uuid1)))
	require.NoError(t, os.Remove(filepath.Join(root, uuid2)))

	// Build absolute paths that contain pciAddr but resolve to our temp devDirs.
	// We use a subdirectory named after pciAddr inside root.
	pciDir := filepath.Join(root, "devices", pciAddr)
	otherDir := filepath.Join(root, "devices", otherPCI)
	require.NoError(t, os.MkdirAll(filepath.Join(pciDir, uuid1), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(otherDir, uuid2), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(pciDir, uuid1, "gpu_instance_id"), []byte("1\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(otherDir, uuid2, "gpu_instance_id"), []byte("3\n"), 0o644))

	// Symlinks whose targets (absolute) contain the respective PCI address.
	require.NoError(t, os.Symlink(filepath.Join(pciDir, uuid1), filepath.Join(root, uuid1)))
	require.NoError(t, os.Symlink(filepath.Join(otherDir, uuid2), filepath.Join(root, uuid2)))

	instances := []MIGInstance{
		{GIID: 1, Profile: MIGProfile{Name: "1g.10gb"}},
	}
	require.NoError(t, enrichMdevPaths(root, pciAddr, instances))
	assert.Equal(t, filepath.Join(root, uuid1), instances[0].MdevPath)
	assert.Equal(t, uuid1, instances[0].UUID)
}

func TestEnrichMdevPaths_MissingMdevBase_ReturnsError(t *testing.T) {
	instances := []MIGInstance{{GIID: 1}}
	err := enrichMdevPaths("/nonexistent/mdev/path", "0000:01:00.0", instances)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mdev subsystem not present")
}

func TestEnrichMdevPaths_NoMatchingUUID_ReturnsError(t *testing.T) {
	root := t.TempDir()
	// Create a symlink for a different GPU — should not match.
	devDir := t.TempDir()
	uuid := "cccccccc-0000-0000-0000-000000000001"
	require.NoError(t, os.MkdirAll(filepath.Join(devDir, "gpu_instance_id"), 0o755))
	require.NoError(t, os.Symlink(devDir, filepath.Join(root, uuid)))

	instances := []MIGInstance{{GIID: 1}}
	err := enrichMdevPaths(root, "0000:01:00.0", instances)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no mdev path found")
}

// --- IsMIGCapable ---

func TestIsMIGCapable(t *testing.T) {
	capable := [][2]string{
		{"10de", "20b2"}, // A100 SXM 40GB
		{"10de", "20b5"}, // A100 SXM 80GB
		{"10de", "20f1"}, // A100 PCIe 40GB
		{"10de", "20f3"}, // A100 PCIe 80GB
		{"10de", "2233"}, // A30
		{"10de", "2330"}, // H100 SXM
		{"10de", "2331"}, // H100 PCIe
		{"10de", "233a"}, // H100 NVL
		{"10de", "2335"}, // H200 SXM
		{"10de", "2bb5"}, // RTX Pro 6000 Blackwell Server Edition
	}
	for _, ids := range capable {
		assert.True(t, IsMIGCapable(ids[0], ids[1]),
			"want MIG capable for %s:%s", ids[0], ids[1])
	}

	notCapable := [][2]string{
		{"10de", "2236"}, // A10 (not MIG capable)
		{"10de", "1e04"}, // RTX 2080 Ti
		{"1002", "687f"}, // AMD Vega 10
		{"8086", "0000"}, // Intel
		{"0000", "0000"}, // unknown
	}
	for _, ids := range notCapable {
		assert.False(t, IsMIGCapable(ids[0], ids[1]),
			"want NOT MIG capable for %s:%s", ids[0], ids[1])
	}
}
