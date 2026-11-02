package cli

import (
	"fmt"
	"os"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/state/clustersize"
)

// Declaring is write-once per process, so whichever test loaded a config first
// would otherwise fix the count for the rest: a multi-node config loaded early
// leaves every one-node embedded JetStream fixture unable to declare.
func TestMain(m *testing.M) {
	if err := clustersize.Declare(1); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to declare cluster size: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
