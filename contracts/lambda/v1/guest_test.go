package lambdav1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseScope(t *testing.T) {
	s, err := ParseScope("ap-southeast-2/000000000001/fn-1/env-1/gen-1")
	require.NoError(t, err)
	assert.Equal(t, "spinifex-lambda://ap-southeast-2/000000000001/fn-1/env-1/gen-1/host", s.URI(RoleHost))
	for _, bad := range []string{
		"", "a/b/c/d", "a/b/c/d/e/f", "a//c/d/e", "a/b/c/d/e/", "a/b/c/d/e?x", "a/b/c/d/e#x", "a b/b/c/d/e",
	} {
		_, err := ParseScope(bad)
		assert.Error(t, err, bad)
	}
}
