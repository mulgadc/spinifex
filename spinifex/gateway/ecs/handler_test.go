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

// Every registered action is the NotImplemented stub in this sprint.
func TestActions_AllNotImplemented(t *testing.T) {
	require.NotEmpty(t, Actions)
	for action, h := range Actions {
		_, err := h(nil, "123456789012", []byte("{}"))
		require.Error(t, err, "action %q", action)
		assert.Equal(t, awserrors.ErrorNotImplemented, err.Error(), "action %q", action)
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
