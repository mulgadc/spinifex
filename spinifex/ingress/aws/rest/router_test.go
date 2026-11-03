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

func TestRouterActionsAreSortedAndDistinct(t *testing.T) {
	rr := NewRouter("svc", []Route[int]{
		{Method: "GET", Pattern: "/b", Action: "GetB"},
		{Method: "DELETE", Pattern: "/tags/*", Action: "UntagResource"},
		{Method: "POST", Pattern: "/a", Action: "CreateA"},
		{Method: "DELETE", Pattern: "/tags/{arn}", Action: "UntagResource"},
	})

	assert.Equal(t, []string{"CreateA", "GetB", "UntagResource"}, rr.Actions())
}
