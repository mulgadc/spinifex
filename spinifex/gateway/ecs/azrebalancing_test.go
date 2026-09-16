package gateway_ecs

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A caller asking for nothing, or asking for the only value this platform can
// honour, is accepted. Anything else is refused rather than dropped, since a
// dropped ENABLED would report a service as rebalancing when nothing does.
func TestCheckAZRebalancing(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "empty body", body: ""},
		{name: "field absent", body: `{"serviceName":"svc"}`},
		{name: "disabled", body: `{"availabilityZoneRebalancing":"DISABLED"}`},
		{name: "unparseable body defers to the SDK shapes", body: `{`},
		{name: "enabled", body: `{"availabilityZoneRebalancing":"ENABLED"}`, wantErr: true},
		{name: "unknown value", body: `{"availabilityZoneRebalancing":"SOMETHING"}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAZRebalancing([]byte(tc.body))
			if !tc.wantErr {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			code, message, ok := awserrors.ResolveErrorDetail(err)
			require.True(t, ok)
			assert.Equal(t, awserrors.ErrorECSInvalidParameter, code)
			assert.Contains(t, message, "availabilityZoneRebalancing")
		})
	}
}

// Every service in a DescribeServices response carries the field, so a plan
// taken straight after an apply reads the value it wrote rather than an empty
// one and stops proposing an update.
func TestAddSDKGapFields_DescribeServices(t *testing.T) {
	out := &ecs.DescribeServicesOutput{Services: []*ecs.Service{
		{ServiceName: aws.String("a")},
		{ServiceName: aws.String("b")},
	}}
	body, err := jsonutil.BuildJSON(out)
	require.NoError(t, err)

	var doc struct {
		Services []map[string]any `json:"services"`
	}
	require.NoError(t, json.Unmarshal(addSDKGapFields(out, body), &doc))
	require.Len(t, doc.Services, 2)
	for _, svc := range doc.Services {
		assert.Equal(t, azRebalancingDisabled, svc[availabilityZoneRebalancingField])
	}
}

// The single-service responses carry the field under "service" rather than
// "services".
func TestAddSDKGapFields_SingleService(t *testing.T) {
	for _, out := range []any{
		&ecs.CreateServiceOutput{Service: &ecs.Service{ServiceName: aws.String("a")}},
		&ecs.UpdateServiceOutput{Service: &ecs.Service{ServiceName: aws.String("a")}},
		&ecs.DeleteServiceOutput{Service: &ecs.Service{ServiceName: aws.String("a")}},
	} {
		body, err := jsonutil.BuildJSON(out)
		require.NoError(t, err)

		var doc struct {
			Service map[string]any `json:"service"`
		}
		require.NoError(t, json.Unmarshal(addSDKGapFields(out, body), &doc))
		assert.Equal(t, azRebalancingDisabled, doc.Service[availabilityZoneRebalancingField])
	}
}

// A response carrying no service is returned byte for byte, so nothing else on
// the ECS surface pays for this.
func TestAddSDKGapFields_LeavesOtherResponsesAlone(t *testing.T) {
	out := &ecs.ListServicesOutput{ServiceArns: aws.StringSlice([]string{"arn:aws:ecs:::service/a"})}
	body, err := jsonutil.BuildJSON(out)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(addSDKGapFields(out, body)))
}

// The re-encode must not rewrite the epoch-second times jsonutil emitted: a
// float64 round trip renders them in exponent form, which the SDK clients then
// read back as a different instant.
func TestAddSDKGapFields_PreservesTimeEncoding(t *testing.T) {
	out := &ecs.DescribeServicesOutput{Services: []*ecs.Service{
		{ServiceName: aws.String("a"), CreatedAt: aws.Time(time.Unix(1757980800, 0).UTC())},
	}}
	body, err := jsonutil.BuildJSON(out)
	require.NoError(t, err)

	var before struct {
		Services []map[string]json.RawMessage `json:"services"`
	}
	require.NoError(t, json.Unmarshal(body, &before))
	var after struct {
		Services []map[string]json.RawMessage `json:"services"`
	}
	require.NoError(t, json.Unmarshal(addSDKGapFields(out, body), &after))

	assert.Equal(t, string(before.Services[0]["createdAt"]), string(after.Services[0]["createdAt"]))
}

// A value the daemon already supplied wins, so this never overwrites a real
// answer once the field has a source.
func TestAddSDKGapFields_KeepsAnExistingValue(t *testing.T) {
	out := &ecs.DescribeServicesOutput{}
	body := []byte(`{"services":[{"serviceName":"a","availabilityZoneRebalancing":"ENABLED"}]}`)

	var doc struct {
		Services []map[string]any `json:"services"`
	}
	require.NoError(t, json.Unmarshal(addSDKGapFields(out, body), &doc))
	assert.Equal(t, "ENABLED", doc.Services[0][availabilityZoneRebalancingField])
}
