package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCABakeCmd_Args(t *testing.T) {
	cmd := caBakeCmd("/img/disk.raw", "/data/config/ca.pem")
	args := cmd.Args

	assert.Equal(t, "virt-customize", filepath.Base(args[0]))
	assert.Contains(t, args, "-a")
	assert.Contains(t, args, "/img/disk.raw")
	assert.Contains(t, args, "--upload")
	assert.Contains(t, args, "/data/config/ca.pem:/tmp/spinifex-ca.pem")
	assert.Contains(t, args, "--run-command")

	// The run-command covers both Debian/Ubuntu/Alpine and RHEL/Rocky trust stores.
	joined := strings.Join(args, " ")
	assert.Contains(t, joined, "update-ca-certificates")
	assert.Contains(t, joined, "update-ca-trust")
}

func TestBakeCACertIntoImage_SkipsWhenCAMissing(t *testing.T) {
	called := false
	orig := caBakeRunner
	caBakeRunner = func(string, string) ([]byte, error) { called = true; return nil, nil }
	defer func() { caBakeRunner = orig }()

	// A missing CA must not invoke virt-customize and must not panic.
	bakeCACertIntoImage("/img/disk.raw", filepath.Join(t.TempDir(), "absent-ca.pem"))
	assert.False(t, called, "runner must not be called when the CA is absent")
}

func TestBakeCACertIntoImage_ContinuesOnRunnerError(t *testing.T) {
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, []byte("ca"), 0600))

	called := false
	orig := caBakeRunner
	caBakeRunner = func(string, string) ([]byte, error) {
		called = true
		return []byte("libguestfs: cannot inspect image"), errors.New("exit status 1")
	}
	defer func() { caBakeRunner = orig }()

	// An image libguestfs cannot inspect must skip-and-continue, never panic/fail.
	bakeCACertIntoImage("/img/disk.raw", caPath)
	assert.True(t, called, "runner must be invoked when the CA is present")
}
