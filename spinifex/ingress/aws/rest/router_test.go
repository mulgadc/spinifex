//test:in-package — patternParams is unexported and fixes the param order every
// handler indexes by.

package rest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPatternParams(t *testing.T) {
	assert.Nil(t, patternParams("/foundation-models"))
	assert.Equal(t, []string{"clusterName"}, patternParams("/clusters/{clusterName}/node-groups"))
	assert.Equal(t, []string{"clusterName", "nodegroupName"},
		patternParams("/clusters/{clusterName}/node-groups/{nodegroupName}"))
	assert.Equal(t, []string{"*"}, patternParams("/tags/*"))
}
