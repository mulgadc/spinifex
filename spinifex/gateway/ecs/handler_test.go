package gateway_ecs

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNotImplemented(t *testing.T) {
	out, err := NotImplemented(nil, "123456789012", []byte("{}"))
	assert.Nil(t, out)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNotImplemented, err.Error())
}

// wiredActions are dispatched to the daemon over NATS; the rest are still the
// NotImplemented stub. Wired actions error here because the nil NATS conn cannot
// reach the daemon — that they no longer return NotImplemented is the assertion.
var wiredActions = map[string]bool{
	"CreateCluster": true, "DeleteCluster": true, "DescribeClusters": true, "ListClusters": true,
	"RegisterTaskDefinition": true, "DeregisterTaskDefinition": true,
	"DescribeTaskDefinition": true, "ListTaskDefinitions": true,
	"RegisterContainerInstance": true, "DeregisterContainerInstance": true,
	"UpdateContainerInstancesState": true, "ProvisionCapacity": true,
	"DescribeContainerInstances": true, "ListContainerInstances": true,
	"RunTask": true, "StartTask": true, "StopTask": true, "DescribeTasks": true, "ListTasks": true,
	"CreateService": true, "UpdateService": true, "DeleteService": true,
	"DescribeServices": true, "ListServices": true,
	"SubmitTaskStateChange": true,
	"PollAssignments":       true,
}

func TestActions_StubsReturnNotImplemented(t *testing.T) {
	require.NotEmpty(t, Actions)
	for action, h := range Actions {
		_, err := h(nil, "123456789012", []byte("{}"))
		require.Error(t, err, "action %q", action)
		if wiredActions[action] {
			assert.NotEqual(t, awserrors.ErrorNotImplemented, err.Error(), "wired action %q should not be a stub", action)
			continue
		}
		assert.Equal(t, awserrors.ErrorNotImplemented, err.Error(), "stub action %q", action)
	}
}

func TestWriteJSONResponse(t *testing.T) {
	w := httptest.NewRecorder()
	WriteJSONResponse(w, map[string]string{"clusterArn": "arn:aws:ecs:::cluster/x"})

	assert.Equal(t, 200, w.Code)
	assert.Equal(t, JSONContentType, w.Header().Get("Content-Type"))

	var got map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "arn:aws:ecs:::cluster/x", got["clusterArn"])
}
