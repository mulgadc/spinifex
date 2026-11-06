package inventory

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListAccountInstancesRefusesAnUnknownNodeSet(t *testing.T) {
	out, err := NewNATSInstances(nil, nil).ListAccountInstances(t.Context(), "000000000000")
	require.Error(t, err)
	assert.Nil(t, out, "an unknown node set must not yield a list")
}
