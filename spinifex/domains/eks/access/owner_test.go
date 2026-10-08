package access

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccountID = "111122223333"
	testPrincipal = "arn:aws:iam::111122223333:role/dev"
)

func newOwnerTestKV(t *testing.T) jetstream.KeyValue {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "access-owner-test"})
	require.NoError(t, err)
	return kv
}

func TestOwnerCreate_DuplicateReturnsErrExistsAndDoesNotOverwrite(t *testing.T) {
	t.Parallel()
	kv := newOwnerTestKV(t)
	owner := New("us-east-1")

	first, err := owner.Create(t.Context(), kv, testAccountID, Spec{
		Cluster: "c1", PrincipalARN: testPrincipal, Username: "alice", Type: EntryTypeStandard,
	})
	require.NoError(t, err)

	_, err = owner.Create(t.Context(), kv, testAccountID, Spec{
		Cluster: "c1", PrincipalARN: testPrincipal, Username: "mallory", Type: EntryTypeStandard,
	})
	require.ErrorIs(t, err, ErrExists)

	after, err := owner.Get(t.Context(), kv, "c1", testPrincipal)
	require.NoError(t, err)
	assert.Equal(t, first, after)
}

func TestOwnerAssociate_ReassociateKeepsOriginalAssociatedAt(t *testing.T) {
	t.Parallel()
	kv := newOwnerTestKV(t)
	owner := New("us-east-1")
	_, err := owner.Create(t.Context(), kv, testAccountID, Spec{Cluster: "c1", PrincipalARN: testPrincipal, Type: EntryTypeStandard})
	require.NoError(t, err)

	const policy = "arn:aws:eks::aws:cluster-access-policy/AmazonEKSViewPolicy"
	first, err := owner.Associate(t.Context(), kv, "c1", testPrincipal, policy, Scope{Type: ScopeCluster})
	require.NoError(t, err)

	second, err := owner.Associate(t.Context(), kv, "c1", testPrincipal, policy, Scope{Type: ScopeCluster})
	require.NoError(t, err)
	assert.Equal(t, first.AssociatedAt, second.AssociatedAt)
}

func TestOwnerDisassociate_UnknownPolicyPerformsNoWrite(t *testing.T) {
	t.Parallel()
	kv := newOwnerTestKV(t)
	owner := New("us-east-1")
	_, err := owner.Create(t.Context(), kv, testAccountID, Spec{Cluster: "c1", PrincipalARN: testPrincipal, Type: EntryTypeStandard})
	require.NoError(t, err)

	before, err := kv.Get(t.Context(), Key("c1", testPrincipal))
	require.NoError(t, err)

	err = owner.Disassociate(t.Context(), kv, "c1", testPrincipal, "arn:aws:eks::aws:cluster-access-policy/AmazonEKSEditPolicy")
	require.NoError(t, err)

	after, err := kv.Get(t.Context(), Key("c1", testPrincipal))
	require.NoError(t, err)
	assert.Equal(t, before.Revision(), after.Revision())
}

func TestOwnerDelete_MissingReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	kv := newOwnerTestKV(t)
	owner := New("us-east-1")
	err := owner.Delete(t.Context(), kv, "c1", testPrincipal)
	require.ErrorIs(t, err, ErrNotFound)
}
