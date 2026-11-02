package nbd

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestQemuTarget_RewritesUnixForm locks the translation: the contract's
// unix URI is not a form qemu accepts, so passing it through unchanged would
// fail at the copy rather than here.
func TestQemuTarget_RewritesUnixForm(t *testing.T) {
	socket := t.TempDir() + "/export.sock"
	require.NoError(t, os.WriteFile(socket, nil, 0o600))

	got, err := qemuTarget("nbd:unix:" + socket)
	require.NoError(t, err)
	assert.Equal(t, "nbd+unix:///?socket="+socket, got)

	got, err = qemuTarget("nbd://10.0.0.5:10809")
	require.NoError(t, err)
	assert.Equal(t, "nbd://10.0.0.5:10809", got)

	_, err = qemuTarget("nbd:unix:" + t.TempDir() + "/absent.sock")
	require.Error(t, err, "a socket that is not there must fail before qemu-img is run")
}
