package vpcd

import (
	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// LocalStateReaderForTest is the local instance state reader's consumer-facing
// shape, exported so an external test can exercise it without reaching into
// package internals.
type LocalStateReaderForTest interface {
	LocalVM(instanceID string) (*vm.VM, error)
}

// NewLocalVMStateReaderForTest builds a reader over dataDir. A non-nil
// readState replaces the parse step, so a test can count parses and prove the
// mtime/size cache is doing its job.
func NewLocalVMStateReaderForTest(dataDir string, readState func(path string) (*daemon.LocalState, error)) LocalStateReaderForTest {
	r := newLocalVMStateReader(dataDir)
	if readState != nil {
		r.readState = readState
	}
	return r
}
