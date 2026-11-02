package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// assertVMPresent asserts that the daemon's VM manager has an entry for id.
func assertVMPresent(t *testing.T, d *Daemon, id string, msgAndArgs ...any) {
	t.Helper()
	_, ok := d.vmMgr.Get(id)
	assert.True(t, ok, msgAndArgs...)
}

// assertVMNotPresent asserts that the daemon's VM manager has no entry for id.
func assertVMNotPresent(t *testing.T, d *Daemon, id string, msgAndArgs ...any) {
	t.Helper()
	_, ok := d.vmMgr.Get(id)
	assert.False(t, ok, msgAndArgs...)
}
