package lbagent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCertTestAgent(t *testing.T) *Agent {
	t.Helper()
	a, err := New("lb-1", "https://gw.local", "AK", "SK", "us-east-1")
	require.NoError(t, err)
	a.certDir = t.TempDir()
	return a
}

func TestWriteCertFiles_WritesPEM0600(t *testing.T) {
	a := newCertTestAgent(t)
	path := filepath.Join(a.certDir, "lb-1-lst.pem")

	err := a.writeCertFiles(a.certDir, []certFile{{Path: path, PEM: "LEAF\nKEY\n"}})
	require.NoError(t, err)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "LEAF\nKEY\n", string(data))

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
}

func TestWriteCertFiles_Empty(t *testing.T) {
	a := newCertTestAgent(t)
	assert.NoError(t, a.writeCertFiles(a.certDir, nil))
}

func TestWriteCertFiles_RejectsTraversal(t *testing.T) {
	a := newCertTestAgent(t)
	cases := []string{
		filepath.Join(a.certDir, "../escape.pem"),
		"/etc/passwd",
		filepath.Join(a.certDir, "sub/dir/x.pem"),
	}
	for _, p := range cases {
		err := a.writeCertFiles(a.certDir, []certFile{{Path: p, PEM: "X"}})
		require.Error(t, err, "path %q must be rejected", p)
		assert.Contains(t, err.Error(), "escapes")
	}
}

func TestWriteCertFiles_RejectsEmptyPEM(t *testing.T) {
	a := newCertTestAgent(t)
	err := a.writeCertFiles(a.certDir, []certFile{{Path: filepath.Join(a.certDir, "x.pem"), PEM: ""}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty PEM")
}
