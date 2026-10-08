package eksv1

import (
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/stretchr/testify/require"
)

var testManifests = []StagedAddonManifest{
	{
		AddonName:             "aws-load-balancer-controller",
		AddonVersion:          "2.11.0",
		ServiceAccountRoleArn: "arn:aws:iam::123456789012:role/alb",
		ConfigurationValues:   `{"replicaCount":2}`,
	},
	{AddonName: "spinifex-noop", AddonVersion: "0.1.0"},
}

// The host-internal NATS form: camelCase tags, empty optionals omitted.
func TestStagedAddonManifestJSONContract(t *testing.T) {
	data, err := json.Marshal(testManifests)
	require.NoError(t, err)
	require.Equal(t, `[`+
		`{"addonName":"aws-load-balancer-controller","addonVersion":"2.11.0",`+
		`"serviceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","configurationValues":"{\"replicaCount\":2}"},`+
		`{"addonName":"spinifex-noop","addonVersion":"0.1.0"}]`,
		string(data))
}

// The deployed guest body, as rendered by the gateway's SDK REST-JSON
// marshaller: Go field names in declaration order, empty strings present.
func TestInternalAddonsResponse_RESTJSONBody(t *testing.T) {
	data, err := jsonutil.BuildJSON(&InternalAddonsResponse{Addons: testManifests})
	require.NoError(t, err)
	require.Equal(t, `{"Addons":[`+
		`{"AddonName":"aws-load-balancer-controller","AddonVersion":"2.11.0",`+
		`"ServiceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","ConfigurationValues":"{\"replicaCount\":2}"},`+
		`{"AddonName":"spinifex-noop","AddonVersion":"0.1.0","ServiceAccountRoleArn":"","ConfigurationValues":""}]}`,
		string(data))

	empty, err := jsonutil.BuildJSON(&InternalAddonsResponse{Addons: []StagedAddonManifest{}})
	require.NoError(t, err)
	require.Equal(t, `{"Addons":[]}`, string(empty))
}

// The tag form encoding/json would emit; not what the gateway sends, but the
// guest must decode both this and the REST-JSON body identically.
func TestInternalAddonsResponse_EncodingJSONForm(t *testing.T) {
	data, err := json.Marshal(InternalAddonsResponse{Addons: testManifests})
	require.NoError(t, err)
	require.Equal(t, `{"addons":[`+
		`{"addonName":"aws-load-balancer-controller","addonVersion":"2.11.0",`+
		`"serviceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","configurationValues":"{\"replicaCount\":2}"},`+
		`{"addonName":"spinifex-noop","addonVersion":"0.1.0"}]}`,
		string(data))
}

func TestInternalAddonsResponse_DecodesBothForms(t *testing.T) {
	for name, body := range map[string]string{
		"rest-json": `{"Addons":[` +
			`{"AddonName":"aws-load-balancer-controller","AddonVersion":"2.11.0",` +
			`"ServiceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","ConfigurationValues":"{\"replicaCount\":2}"},` +
			`{"AddonName":"spinifex-noop","AddonVersion":"0.1.0","ServiceAccountRoleArn":"","ConfigurationValues":""}]}`,
		"encoding-json": `{"addons":[` +
			`{"addonName":"aws-load-balancer-controller","addonVersion":"2.11.0",` +
			`"serviceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","configurationValues":"{\"replicaCount\":2}"},` +
			`{"addonName":"spinifex-noop","addonVersion":"0.1.0"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var got InternalAddonsResponse
			require.NoError(t, json.Unmarshal([]byte(body), &got))
			require.Equal(t, InternalAddonsResponse{Addons: testManifests}, got)
		})
	}
}
