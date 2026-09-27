//test:in-package — ForgetSuperseded is asserted against Manager's own map and
//the in-package fake deps, which is where the rest of the shutdown suite lives.

package vm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A returning node has to drop the instances the cluster moved while it was
// away, before its restore relaunches them from its own state file.
func TestForgetSuperseded_RemovesTheLocalCopy(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := NewManager()
	m.SetDeps(Deps{NodeID: "node-1", StateStore: newFakeStateStore()})

	instance := &VM{ID: "i-moved", InstanceType: "t3.micro"}
	m.Insert(instance)
	_, present := m.Get("i-moved")
	require.True(t, present)

	m.ForgetSuperseded(instance)

	_, present = m.Get("i-moved")
	assert.False(t, present,
		"an instance another node owns now must not survive in this node's view")
}

// Called on whatever the caller found, and what it found may be nothing.
func TestForgetSuperseded_NilIsANoOp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	m := NewManager()

	assert.NotPanics(t, func() { m.ForgetSuperseded(nil) })
}
