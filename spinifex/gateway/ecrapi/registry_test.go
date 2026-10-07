package gateway_ecrapi_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	gateway_ecrapi "github.com/mulgadc/spinifex/spinifex/gateway/ecrapi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// describeRegistryWire runs the dispatched DescribeRegistry handler and
// serialises its output the way the gateway does, returning the decoded body.
func describeRegistryWire(t *testing.T, accountID string, body []byte) map[string]any {
	t.Helper()
	out, err := gateway_ecrapi.Actions["DescribeRegistry"](context.Background(), nil, accountID, body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	gateway_ecrapi.WriteJSONResponse(w, out)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &decoded))
	return decoded
}

// AWS answers an account with no replication with the configuration present
// and an empty rules list, not with the configuration omitted.
func TestDescribeRegistry_CallerAccountAndEmptyReplication(t *testing.T) {
	for _, body := range []string{"", "{}", "null"} {
		t.Run("body="+body, func(t *testing.T) {
			decoded := describeRegistryWire(t, "123456789012", []byte(body))
			assert.Equal(t, map[string]any{
				"registryId":               "123456789012",
				"replicationConfiguration": map[string]any{"rules": []any{}},
			}, decoded)
		})
	}
}

func TestDescribeRegistry_ScopedToCaller(t *testing.T) {
	a := describeRegistryWire(t, "111111111111", nil)
	b := describeRegistryWire(t, "222222222222", nil)
	assert.Equal(t, "111111111111", a["registryId"])
	assert.Equal(t, "222222222222", b["registryId"])
}

func TestDescribeRegistry_MalformedBody(t *testing.T) {
	for _, body := range []string{"{", "[]", `"x"`} {
		t.Run(body, func(t *testing.T) {
			out, err := gateway_ecrapi.DescribeRegistry(context.Background(), nil, "123456789012", []byte(body))
			assert.Nil(t, out)
			require.Error(t, err)
			assert.Equal(t, awserrors.ValidErrorCodeFromError(gateway_ecrapi.MalformedBodyError()),
				awserrors.ValidErrorCodeFromError(err))
		})
	}
}

func TestDescribeRegistry_NotStubbed(t *testing.T) {
	assert.NotContains(t, gateway_ecrapi.StubbedActionNames(), "DescribeRegistry")
}
