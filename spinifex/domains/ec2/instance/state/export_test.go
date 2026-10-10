package state

import (
	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// LocalVMReaderForTest is the local instance state reader's consumer-facing
// shape, exported so an external test can exercise it without reaching into
// package internals.
type LocalVMReaderForTest interface {
	LocalVM(instanceID string) (*vm.VM, error)
}

// NewLocalVMReaderForTest builds a reader over dataDir. A non-nil readState
// replaces the parse step, so a test can count parses and prove the
// mtime/size cache is doing its job.
func NewLocalVMReaderForTest(dataDir string, readState func(path string) (*daemon.LocalState, error)) LocalVMReaderForTest {
	r := NewLocalVMReader(dataDir)
	if readState != nil {
		r.readState = readState
	}
	return r
}
