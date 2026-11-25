package vpc

import (
	"context"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// In-package fixtures the external test package needs.
const TestAccountID = testAccountID

var (
	SetupTestVPCService = setupTestVPCService
	CreateTestVPC       = createTestVPC
	CreateTestSubnet    = createTestSubnet
)

// NewPooledTestNodes returns a connection to a pooled JetStream server and n
// services on it, each on its own connection, standing in for cluster nodes.
func NewPooledTestNodes(t *testing.T, n int) (*nats.Conn, []*VPCServiceImpl, []*nats.Conn) {
	t.Helper()
	p, nc := acquireJetStream(t)
	svcs := make([]*VPCServiceImpl, n)
	conns := make([]*nats.Conn, n)
	for i := range n {
		conns[i] = p.connect(t)
		var err error
		svcs[i], err = NewVPCServiceImplWithNATS(t.Context(), nil, conns[i])
		require.NoError(t, err)
	}
	require.NoError(t, p.recordBuckets(t.Context(), testutil.NewJetStream(t, nc)))
	return nc, svcs, conns
}

// FakeCentralTagStore records the tag map passed to PutResourceTags per
// resource and the ids passed to DeleteAllTags, and can be made to fail so
// tests can prove a central-store error never fails the operation it tracks.
type FakeCentralTagStore struct {
	Calls   map[string]map[string]string
	Deleted []string
	Err     error
}

var _ CentralTagStore = (*FakeCentralTagStore)(nil)

func (f *FakeCentralTagStore) PutResourceTags(_ context.Context, _, resourceID string, tags map[string]string) error {
	if f.Calls == nil {
		f.Calls = map[string]map[string]string{}
	}
	f.Calls[resourceID] = tags
	return f.Err
}

func (f *FakeCentralTagStore) DeleteAllTags(_ context.Context, _, resourceID string) error {
	f.Deleted = append(f.Deleted, resourceID)
	return f.Err
}
