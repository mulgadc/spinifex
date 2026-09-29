package gpu

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNvidiaSmi writes a fake nvidia-smi shell script to tmpDir/nvidia-smi,
// prepends tmpDir to PATH, and restores both on test cleanup.
// The script handles these command patterns:
//   - "-i <addr> -mig 1" → enables MIG (exit 0)
//   - "-i <addr> -mig 0" → disables MIG (exit 0)
//   - "--query-gpu=mig.mode.current ..." → returns "Enabled"
//   - "mig -lgip -i <addr>" → returns lgipOutput
//   - "mig -lgi -i <addr>" → returns lgiOutput
//   - "mig -cgi <id> -i <addr>" → creates a GPU instance (up to maxGIs, then capacity error)
//   - "mig -cci ..." → creates a compute instance
//   - "mig -dgi ..." → destroys all instances
//
// maxGIs controls how many GI create calls succeed before a capacity-exhausted error.
func fakeNvidiaSmi(t *testing.T, tmpDir, lgipOutput, lgiOutput string, maxGIs int) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "lgip.txt"), []byte(lgipOutput), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "lgi.txt"), []byte(lgiOutput), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tmpDir, "max_gis"), []byte(strconv.Itoa(maxGIs)), 0o644))

	script := `#!/bin/sh
STATE="` + tmpDir + `"
case "$*" in
  -i\ *\ -mig\ 1)
    echo "MIG mode enabled"; exit 0 ;;
  -i\ *\ -mig\ 0)
    echo "MIG mode disabled"; exit 0 ;;
  --query-gpu=mig.mode.current\ *)
    echo "Enabled"; exit 0 ;;
  mig\ -lgip\ *)
    cat "$STATE/lgip.txt"; exit 0 ;;
  mig\ -lgi\ *)
    cat "$STATE/lgi.txt"; exit 0 ;;
  mig\ -cgi\ *)
    COUNT=$(cat "$STATE/cgi_count" 2>/dev/null || echo 0)
    COUNT=$((COUNT + 1))
    MAX=$(cat "$STATE/max_gis")
    if [ "$COUNT" -le "$MAX" ]; then
      printf "Successfully created GPU instance ID  %s on GPU  0 using profile MIG 1g.10gb\n" "$COUNT"
      printf "%s" "$COUNT" > "$STATE/cgi_count"
      exit 0
    fi
    printf "No space left\n"; exit 1 ;;
  mig\ -cci\ -gi\ *)
    echo "Successfully created compute instance ID  0 on GPU  0 GPU instance ID  1"; exit 0 ;;
  mig\ -dgi\ *)
    echo "All GPU instances deleted"; exit 0 ;;
  *)
    exit 1 ;;
esac
`
	smiPath := filepath.Join(tmpDir, "nvidia-smi")
	require.NoError(t, os.WriteFile(smiPath, []byte(script), 0o755))
	t.Setenv("PATH", tmpDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// makeFakeMdev creates a fake mdev device directory structure that enrichMdevPaths can parse.
// It returns the mdev base path (to set mdevBasePath to in tests) and the UUID.
// The symlink from basePath/uuid points to a directory whose absolute path contains pciAddr.
func makeFakeMdev(t *testing.T, pciAddr string, giID int) (basePath, uuid string) {
	t.Helper()
	root := t.TempDir()

	uuid = "aaaabbbb-0000-0000-0000-" + strconv.Itoa(giID) + "00000000000"

	// Build a directory path that contains pciAddr so Readlink returns a target with pciAddr.
	devDir := filepath.Join(root, "devices", pciAddr, uuid)
	require.NoError(t, os.MkdirAll(devDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(devDir, "gpu_instance_id"),
		[]byte(strconv.Itoa(giID)+"\n"),
		0o644,
	))

	// The symlink lives in root (which is mdevBasePath) and points to devDir
	// (which contains pciAddr in its absolute path).
	basePath = filepath.Join(root, "mdev")
	require.NoError(t, os.MkdirAll(basePath, 0o755))
	require.NoError(t, os.Symlink(devDir, filepath.Join(basePath, uuid)))
	return basePath, uuid
}

const lgipA100 = `
+-------------------------------------------------------------------------------------------+
|   0  MIG 1g.10gb  Profile  ID: 19,  Instances: 7/7   Mem: 9.62 GiB                      |
+-------------------------------------------------------------------------------------------+
`

// lgipA100Slow is an alias for the same fixture used by manager_mig slow-path tests.
const lgipA100Slow = lgipA100

// --- EnableMIGMode ---

func TestEnableMIGMode_Success(t *testing.T) {
	dir := t.TempDir()
	fakeNvidiaSmi(t, dir, lgipA100, "", 0)
	require.NoError(t, EnableMIGMode("0000:01:00.0"))
}

func TestEnableMIGMode_Failure(t *testing.T) {
	// No fake smi — nvidia-smi not found → error.
	t.Setenv("PATH", t.TempDir())
	err := EnableMIGMode("0000:01:00.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "enable MIG mode")
}

// --- DisableMIGMode ---

func TestDisableMIGMode_Success(t *testing.T) {
	dir := t.TempDir()
	fakeNvidiaSmi(t, dir, lgipA100, "", 0)
	require.NoError(t, DisableMIGMode("0000:01:00.0"))
}

func TestDisableMIGMode_Failure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := DisableMIGMode("0000:01:00.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disable MIG mode")
}

// --- IsMIGModeEnabled ---

func TestIsMIGModeEnabled_ReturnsEnabled(t *testing.T) {
	dir := t.TempDir()
	fakeNvidiaSmi(t, dir, lgipA100, "", 0)
	enabled, err := IsMIGModeEnabled("0000:01:00.0")
	require.NoError(t, err)
	assert.True(t, enabled)
}

func TestIsMIGModeEnabled_Failure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := IsMIGModeEnabled("0000:01:00.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query MIG mode")
}

// --- DestroyAllInstances ---

func TestDestroyAllInstances_Success(t *testing.T) {
	dir := t.TempDir()
	fakeNvidiaSmi(t, dir, lgipA100, "", 0)
	require.NoError(t, DestroyAllInstances("0000:01:00.0"))
}

func TestDestroyAllInstances_Failure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := DestroyAllInstances("0000:01:00.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "destroy MIG instances")
}

// --- ListProfiles ---

func TestListProfiles_Success(t *testing.T) {
	dir := t.TempDir()
	fakeNvidiaSmi(t, dir, lgipA100, "", 0)
	profiles, err := ListProfiles("0000:01:00.0")
	require.NoError(t, err)
	require.Len(t, profiles, 1)
	assert.Equal(t, "1g.10gb", profiles[0].Name)
	assert.Equal(t, 19, profiles[0].ID)
}

func TestListProfiles_Failure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ListProfiles("0000:01:00.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nvidia-smi mig -lgip")
}

// --- CreateInstances ---

func TestCreateInstances_Success(t *testing.T) {
	const pciAddr = "0000:01:00.0"
	dir := t.TempDir()

	// Set up fake mdev for GI 1.
	mdevBase, uuid := makeFakeMdev(t, pciAddr, 1)
	mdevBasePath = mdevBase
	t.Cleanup(func() { mdevBasePath = "/sys/bus/mdev/devices" })

	fakeNvidiaSmi(t, dir, lgipA100, "", 1) // maxGIs=1: one success then capacity error

	profile := MIGProfile{ID: 19, Name: "1g.10gb", MemoryMiB: 10240}
	instances, err := CreateInstances(pciAddr, profile)
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, 1, instances[0].GIID)
	assert.Equal(t, "1g.10gb", instances[0].Profile.Name)
	assert.Equal(t, uuid, instances[0].UUID)
	assert.NotEmpty(t, instances[0].MdevPath)
}

func TestCreateInstances_NoGPUInstances_Failure(t *testing.T) {
	const pciAddr = "0000:01:00.0"
	dir := t.TempDir()
	fakeNvidiaSmi(t, dir, lgipA100, "", 0) // maxGIs=0: immediately capacity error with no instances created

	profile := MIGProfile{ID: 19, Name: "1g.10gb", MemoryMiB: 10240}
	_, err := CreateInstances(pciAddr, profile)
	// With 0 GIs created, the capacity error is treated as an actual error (not normal loop exit).
	require.Error(t, err)
}

// --- ListInstances ---

const lgiA100OneSlice = `
+--------------------------------------------------------------------+
| Existing GPU Instances on GPU 0                                    |
|====================================================================|
|   0   1  MIG 1g.10gb    0:1   P2P: No                            |
+--------------------------------------------------------------------+
`

func TestListInstances_Success(t *testing.T) {
	const pciAddr = "0000:01:00.0"
	dir := t.TempDir()

	mdevBase, uuid := makeFakeMdev(t, pciAddr, 1)
	mdevBasePath = mdevBase
	t.Cleanup(func() { mdevBasePath = "/sys/bus/mdev/devices" })

	fakeNvidiaSmi(t, dir, lgipA100, lgiA100OneSlice, 0)
	instances, err := ListInstances(pciAddr)
	require.NoError(t, err)
	require.Len(t, instances, 1)
	assert.Equal(t, 1, instances[0].GIID)
	assert.Equal(t, "1g.10gb", instances[0].Profile.Name)
	assert.Equal(t, uuid, instances[0].UUID)
}

func TestListInstances_Failure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := ListInstances("0000:01:00.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nvidia-smi mig -lgi")
}
