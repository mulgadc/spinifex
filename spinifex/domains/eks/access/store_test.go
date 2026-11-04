package access

import (
	"testing"

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
