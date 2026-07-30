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

func TestPruneCertFiles_RemovesUndeliveredPEM(t *testing.T) {
	a := newCertTestAgent(t)
	kept := filepath.Join(a.certDir, "lb-1-lst-1.pem")
	stale := filepath.Join(a.certDir, "lb-1-lst-1-cert_bbbb.pem")
	require.NoError(t, a.writeCertFiles(a.certDir, []certFile{
		{Path: kept, PEM: "LEAF\nKEY\n"},
		{Path: stale, PEM: "OTHER\nKEY\n"},
	}))

	// The next poll delivers only the default cert, as after a detach.
	require.NoError(t, a.pruneCertFiles(a.certDir, []certFile{{Path: kept, PEM: "LEAF\nKEY\n"}}))

	assert.FileExists(t, kept)
	_, err := os.Stat(stale)
	assert.True(t, os.IsNotExist(err), "a detached certificate's private key must not be left on disk")
}

// A listener switched from HTTPS to HTTP delivers no certs at all, which is the
// case writeCertFiles short-circuits — pruning still has to run.
func TestPruneCertFiles_EmptyDeliverySetClearsDir(t *testing.T) {
	a := newCertTestAgent(t)
	stale := filepath.Join(a.certDir, "lb-1-lst-1.pem")
	require.NoError(t, a.writeCertFiles(a.certDir, []certFile{{Path: stale, PEM: "LEAF\nKEY\n"}}))

	require.NoError(t, a.pruneCertFiles(a.certDir, nil))

	_, err := os.Stat(stale)
	assert.True(t, os.IsNotExist(err))
}

func TestPruneCertFiles_LeavesNonPEMAndMissingDirAlone(t *testing.T) {
	a := newCertTestAgent(t)
	other := filepath.Join(a.certDir, "haproxy.cfg")
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o600))

	require.NoError(t, a.pruneCertFiles(a.certDir, nil))
	assert.FileExists(t, other, "pruning must not touch an engine's own files")

	a.certDir = filepath.Join(t.TempDir(), "does-not-exist")
	assert.NoError(t, a.pruneCertFiles(a.certDir, nil), "a dir that was never created is not an error")
}
