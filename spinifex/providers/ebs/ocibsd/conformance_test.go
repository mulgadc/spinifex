package ocibsd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/conformance"
	"github.com/mulgadc/spinifex/spinifex/providers/ebs/natsserve"
)

func TestConformanceInProcess(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		c := newCluster(t)
		return settling{EBSProvider: c.router(), fake: c.fake}
	})
}

// Each node serves its own per-node subjects and joins the shared queue, as
// the per-node service role does.
func TestConformanceOverNATS(t *testing.T) {
	conformance.RunSuite(t, func(t *testing.T) ebsprovider.EBSProvider {
		t.Helper()
		c := newCluster(t)
		_, conn := testutil.StartTestNATS(t)
		for node, p := range c.nodes {
			stop, err := natsserve.Serve(t.Context(), conn, settling{EBSProvider: p, fake: c.fake}, natsserve.Options{NodeID: node})
			require.NoError(t, err)
			t.Cleanup(stop)
		}
		return ebsprovider.NewNATSProvider(conn, 5*time.Second)
	})
}
