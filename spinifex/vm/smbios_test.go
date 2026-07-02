package vm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecute_EmitsEC2SMBIOS(t *testing.T) {
	uuid := ec2SMBIOSUUID("i-0123456789abcdef0")
	cfg := Config{
		CPUCount:           2,
		Memory:             1024,
		Architecture:       "x86_64",
		Drives:             []Drive{{File: "disk.img", Format: "qcow2"}},
		SMBIOSUUID:         uuid,
		SMBIOSManufacturer: "Amazon EC2",
		SMBIOSAssetTag:     "Amazon EC2",
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)
	args := cmd.Args[1:]

	assert.Equal(t, uuid, argValue(args, "-uuid"))

	// Both -smbios entries must be present: type=1 manufacturer/serial, type=3 asset.
	smbios := allArgValues(args, "-smbios")
	assert.Contains(t, smbios, "type=1,manufacturer=Amazon EC2,serial="+uuid)
	assert.Contains(t, smbios, "type=3,asset=Amazon EC2")
}

func TestExecute_NoSMBIOSWhenUnset(t *testing.T) {
	cfg := Config{
		CPUCount:     2,
		Memory:       1024,
		Architecture: "x86_64",
		Drives:       []Drive{{File: "disk.img", Format: "qcow2"}},
	}

	cmd, err := cfg.Execute()
	require.NoError(t, err)
	args := cmd.Args[1:]

	assert.False(t, argExists(args, "-uuid"), "no -uuid expected when SMBIOSUUID unset")
	assert.False(t, argExists(args, "-smbios"), "no -smbios expected when SMBIOS fields unset")
}

func TestEC2SMBIOSUUID(t *testing.T) {
	id := "i-0123456789abcdef0"
	u := ec2SMBIOSUUID(id)

	// cloud-init's identify_aws keys on an "ec2"-prefixed product_uuid.
	assert.True(t, strings.HasPrefix(u, "ec2"), "uuid must be ec2-prefixed: %s", u)

	// Canonical 8-4-4-4-12 UUID shape.
	assert.Len(t, u, 36)
	parts := strings.Split(u, "-")
	require.Len(t, parts, 5)
	assert.Equal(t, []int{8, 4, 4, 4, 12}, []int{len(parts[0]), len(parts[1]), len(parts[2]), len(parts[3]), len(parts[4])})

	// Deterministic across calls (stable across guest reboot), unique per instance.
	assert.Equal(t, u, ec2SMBIOSUUID(id))
	assert.NotEqual(t, u, ec2SMBIOSUUID("i-ffffffffffffffff0"))
}

func TestBuildBaseVMConfig_SetsEC2SMBIOS(t *testing.T) {
	cfg := buildBaseVMConfig("i-0123456789abcdef0", "t3.micro", "/run/pid", "/run/console.log", "/run/serial.sock", "x86_64", "bios", 2, 1024)

	assert.Equal(t, ec2SMBIOSUUID("i-0123456789abcdef0"), cfg.SMBIOSUUID)
	assert.Equal(t, "Amazon EC2", cfg.SMBIOSManufacturer)
	assert.Equal(t, "Amazon EC2", cfg.SMBIOSAssetTag)
}
