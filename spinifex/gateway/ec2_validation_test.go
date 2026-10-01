//test:in-package — walks the unexported ec2Actions route table and drives
// EC2_Request through the gateway's unexported test helpers.

package gateway

import (
	"encoding/xml"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An action added to the route table with TagSpecifications must say which
// resource types it tags, or a wrong type would be silently dropped again.
func TestEC2TaggableTypes_CoverEveryAction(t *testing.T) {
	for action, handler := range ec2Actions {
		input, err := handler.parse(map[string]string{"Action": action})
		require.NoError(t, err, action)
		_, tags := reflect.TypeOf(input).Elem().FieldByName("TagSpecifications")
		_, listed := ec2TaggableTypes[action]
		assert.Equal(t, tags, listed, "%s: TagSpecifications modelled=%v, listed in ec2TaggableTypes=%v", action, tags, listed)
	}
	for action := range ec2TaggableTypes {
		assert.Contains(t, ec2Actions, action, "ec2TaggableTypes lists an action the gateway does not route")
	}
}

func tagSpecBody(action, resourceType string) string {
	return "Action=" + action + "&TagSpecification.1.ResourceType=" + resourceType +
		"&TagSpecification.1.Tag.1.Key=k&TagSpecification.1.Tag.1.Value=v"
}

func TestEC2Request_TagSpecificationResourceType(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}

	cases := []struct {
		name, body, code, resourceType string
	}{
		{"out of enum", tagSpecBody("CreateKeyPair", "bogus-type") + "&KeyName=k", "InvalidParameterValue", "bogus-type"},
		{"another resource's type", tagSpecBody("CreateInternetGateway", "instance"), "InvalidParameterValue", "instance"},
		{"InvalidParameter action", tagSpecBody("CreateVpc", "instance") + "&CidrBlock=10.0.0.0/16", "InvalidParameter", "instance"},
		{"checked before DryRun", tagSpecBody("CreateVpc", "subnet") + "&CidrBlock=10.0.0.0/16&DryRun=true", "InvalidParameter", "subnet"},
		{"second spec checked", tagSpecBody("RunInstances", "volume") +
			"&TagSpecification.2.ResourceType=image&ImageId=ami-1&MinCount=1&MaxCount=1", "InvalidParameterValue", "image"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveEC2(gw, tc.body)
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "<Code>"+tc.code+"</Code>")
			assert.Contains(t, w.Body.String(),
				"<Message>&#39;"+tc.resourceType+"&#39; is not a valid taggable resource type for this operation.</Message>")
		})
	}
}

// Each of an action's own types reaches the handler, which fails only on the
// absent NATS connection.
func TestEC2Request_TagSpecificationAllowedTypes(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}

	for _, rt := range []string{"instance", "volume", "network-interface", "spot-instances-request"} {
		assertPermitted(t, dispatchEC2(t, gw, tagSpecBody("RunInstances", rt)+"&ImageId=ami-1&MinCount=1&MaxCount=1"))
	}
	assertPermitted(t, dispatchEC2(t, gw, tagSpecBody("CreateVpc", "vpc")+"&CidrBlock=10.0.0.0/16"))
}

// xmlText escapes s as the error body renders it.
func xmlText(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, xml.EscapeText(&b, []byte(s)))
	return b.String()
}

// The refusals are AWS's own, captured by scripts/aws-compliance/aws-response-diff/scen_e.py.
func TestEC2Request_MaxResultsOutOfRange(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}

	cases := []struct {
		action string
		n      int
		code   string
		msg    string
	}{
		{"DescribeCapacityReservations", 0, "InvalidParameterValue", "0 is an invalid value for MaxResults. MaxResults must be null, or between 1 and 1000."},
		{"DescribeCapacityReservations", 1001, "InvalidParameterValue", "1001 is an invalid value for MaxResults. MaxResults must be null, or between 1 and 1000."},
		{"DescribeEgressOnlyInternetGateways", 4, "InvalidParameterValue", "Value (4) for parameter maxResults is invalid. Expecting a value greater than 5."},
		{"DescribeEgressOnlyInternetGateways", 256, "InvalidParameterValue", "Value (256) for parameter maxResults is invalid. Expecting a value less than 255."},
		{"DescribeInstances", 2, "InvalidParameterValue", "Value ( 2 ) for parameter maxResults is invalid. Expecting a value greater than 5."},
		{"DescribeInstanceCreditSpecifications", 4, "InvalidRequest", "The value 4 for the maxResults parameter must be between 5 and 1000. Change the value and try again."},
		{"DescribeInstanceCreditSpecifications", 1001, "InvalidRequest", "The value 1001 for the maxResults parameter must be between 5 and 1000. Change the value and try again."},
		{"DescribeInstanceTypes", 4, "InvalidMaxResults", "Value ( 4 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 100 )."},
		{"DescribeInstanceTypes", 101, "InvalidMaxResults", "Value ( 101 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 100 )."},
		{"DescribeInstanceTypeOfferings", 4, "InvalidMaxResults", "Value ( 4 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 1000 )."},
		{"DescribeInstanceTypeOfferings", 1001, "InvalidMaxResults", "Value ( 1001 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 1000 )."},
		{"DescribeLaunchTemplates", 0, "InvalidParameterValue", "Maximum results allowed are between 1 and 200"},
		{"DescribeLaunchTemplates", 201, "InvalidParameterValue", "Maximum results allowed are between 1 and 200"},
		{"DescribeNatGateways", 4, "InvalidParameter", "1 validation error detected: Value '4' at 'maxResults' failed to satisfy constraint: Member must have value greater than or equal to 5"},
		{"DescribeNatGateways", 1001, "InvalidParameter", "1 validation error detected: Value '1001' at 'maxResults' failed to satisfy constraint: Member must have value less than or equal to 1000"},
	}
	for _, tc := range cases {
		t.Run(tc.action+"/"+strconv.Itoa(tc.n), func(t *testing.T) {
			w := serveEC2(gw, "Action="+tc.action+"&MaxResults="+strconv.Itoa(tc.n))
			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Contains(t, w.Body.String(), "<Code>"+tc.code+"</Code>")
			assert.Contains(t, w.Body.String(), "<Message>"+xmlText(t, tc.msg)+"</Message>")
		})
	}
}

// Captured from AWS: the combination is refused even when MaxResults is also
// out of range.
func TestEC2Request_DescribeInstancesMaxResultsWithInstanceIds(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}

	for _, n := range []string{"2", "5"} {
		w := serveEC2(gw, "Action=DescribeInstances&InstanceId.1=i-0123456789abcdef0&MaxResults="+n)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "<Code>InvalidParameterCombination</Code>")
		assert.Contains(t, w.Body.String(), "<Message>The parameter instancesSet cannot be used with the parameter maxResults</Message>")
	}
}

func TestEC2Request_MaxResultsInRange(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}

	for _, body := range []string{
		"Action=DescribeCapacityReservations&MaxResults=1",
		"Action=DescribeEgressOnlyInternetGateways&MaxResults=255",
		"Action=DescribeInstances&MaxResults=5",
		"Action=DescribeInstances&MaxResults=5000",
		"Action=DescribeInstanceTypes&MaxResults=100",
		"Action=DescribeInstanceTypeOfferings&MaxResults=5",
		"Action=DescribeLaunchTemplates&MaxResults=200",
		"Action=DescribeNatGateways&MaxResults=1000",
		// AWS accepted 4 and 1001 here although the model bounds it 5-1000.
		"Action=DescribeIamInstanceProfileAssociations&MaxResults=4",
		"Action=DescribeIamInstanceProfileAssociations&MaxResults=1001",
	} {
		assertPermitted(t, dispatchEC2(t, gw, body))
	}
}

// AWS answers DryRun ahead of a MaxResults out of range.
func TestEC2Request_MaxResultsAfterDryRun(t *testing.T) {
	gw := &GatewayConfig{DisableLogging: true, Region: authzRegion, IAMService: allowAllIAMService()}
	assertDryRun(t, dispatchEC2(t, gw, "Action=DescribeLaunchTemplates&MaxResults=0&DryRun=true"))
}
