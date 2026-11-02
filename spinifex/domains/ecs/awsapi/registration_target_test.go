package awsapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestActionFromTarget(t *testing.T) {
	assert.Equal(t, "RunTask", actionFromTarget("AmazonEC2ContainerServiceV20141113.RunTask"))
	assert.Equal(t, "CreateCluster", actionFromTarget("CreateCluster"))
	assert.Empty(t, actionFromTarget(""))
}
