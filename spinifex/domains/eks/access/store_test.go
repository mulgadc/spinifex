package access

import (
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyPaths(t *testing.T) {
	assert.Equal(t, "clusters/alpha/access-entries/", Prefix("alpha"))
	assert.Equal(t,
		"clusters/alpha/access-entries/"+PrincipalARNHash("arn:aws:iam::111122223333:user/dev"),
		Key("alpha", "arn:aws:iam::111122223333:user/dev"))
}

// The record-store guard clauses reject malformed input before touching the KV,
// so a nil handle is enough to exercise them.
func TestPutGetListGuards(t *testing.T) {
	t.Parallel()
	require.Error(t, put(t.Context(), nil, nil))
	require.Error(t, put(t.Context(), nil, &Record{PrincipalARN: "arn:aws:iam::000000000001:user/admin"}))

	_, err := Get(t.Context(), nil, "", "arn:aws:iam::000000000001:user/admin")
	require.Error(t, err)

	_, err = List(t.Context(), nil, "")
	require.Error(t, err)
}

// A writer that wins every race spends the attempt budget, and the error still names the conflict.
func TestCasUpdate_ExhaustedWrapsRevisionMismatch(t *testing.T) {
	t.Parallel()
	kv := newOwnerTestKV(t)
	rec := &Record{ClusterName: "c1", PrincipalARN: testPrincipal, KubernetesUsername: "alice"}
	require.NoError(t, put(t.Context(), kv, rec))

	_, err := casUpdate(t.Context(), kv, "c1", testPrincipal, func(r *Record) bool {
		require.NoError(t, put(t.Context(), kv, rec))
		r.KubernetesUsername = "bob"
		return true
	})
	require.ErrorIs(t, err, jetstream.ErrKeyRevisionMismatch)
}
