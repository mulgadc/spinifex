//test:in-package — the embedded nats.conf template it checks is unexported,
// and exporting it would widen the package's API for a test's convenience.

package cmd

import (
	"strings"
	"testing"

	natssvc "github.com/mulgadc/spinifex/spinifex/runtime/roles/nats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNATSConfServerNameMatchesTheHelper ties the rendered server_name to
// nats.ServerName. JetStream reports stream peers by server name, so anything
// mapping a replica back to the node holding it reads the prefix from that
// helper — and a template that drifted from it would break the mapping silently,
// reporting a healthy cluster as one whose buckets name no node at all.
func TestNATSConfServerNameMatchesTheHelper(t *testing.T) {
	var line string
	for l := range strings.SplitSeq(natsConfTemplate, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "server_name:") {
			line = strings.TrimSpace(l)
			break
		}
	}
	require.NotEmpty(t, line, "nats.conf no longer sets server_name")

	got := strings.TrimSpace(strings.TrimPrefix(line, "server_name:"))
	assert.Equal(t, natssvc.ServerName("{{.Node}}"), got)

	node, ok := natssvc.NodeFromServerName(natssvc.ServerName("node2"))
	assert.True(t, ok)
	assert.Equal(t, "node2", node)

	_, ok = natssvc.NodeFromServerName("node2")
	assert.False(t, ok, "a bare node name is not a server name")

	// The bare prefix is a name we could not have generated, and mapping it to the
	// empty node would put a phantom peer into anything counting nodes.
	_, ok = natssvc.NodeFromServerName(natssvc.ServerNamePrefix)
	assert.False(t, ok, "the prefix with no node after it is not a server name")
}
