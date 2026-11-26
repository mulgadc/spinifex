//test:in-package — walks the unexported ec2Actions route table.

package awsapi

import (
	"math"
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"

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
		assert.Contains(t, ec2Actions, action, "ec2TaggableTypes lists an action the registration does not route")
	}
}

// Each ranged action accepts both bounds and refuses one past each with AWS's
// code and text. A max of MaxInt64 has no value past it.
func TestValidateMaxResults_EveryRangeAtBothBounds(t *testing.T) {
	type refusal struct {
		n    int64
		code string
		msg  string
	}
	cases := map[string]struct {
		accept []int64
		refuse []refusal
	}{
		"DescribeCapacityReservations": {[]int64{1, 1000}, []refusal{
			{0, awserrors.ErrorInvalidParameterValue, "0 is an invalid value for MaxResults. MaxResults must be null, or between 1 and 1000."},
			{1001, awserrors.ErrorInvalidParameterValue, "1001 is an invalid value for MaxResults. MaxResults must be null, or between 1 and 1000."},
		}},
		"DescribeEgressOnlyInternetGateways": {[]int64{5, 255}, []refusal{
			{4, awserrors.ErrorInvalidParameterValue, "Value (4) for parameter maxResults is invalid. Expecting a value greater than 5."},
			{256, awserrors.ErrorInvalidParameterValue, "Value (256) for parameter maxResults is invalid. Expecting a value less than 255."},
		}},
		"DescribeInstances": {[]int64{5, math.MaxInt64}, []refusal{
			{4, awserrors.ErrorInvalidParameterValue, "Value ( 4 ) for parameter maxResults is invalid. Expecting a value greater than 5."},
		}},
		"DescribeInstanceCreditSpecifications": {[]int64{5, 1000}, []refusal{
			{4, awserrors.ErrorInvalidRequest, "The value 4 for the maxResults parameter must be between 5 and 1000. Change the value and try again."},
			{1001, awserrors.ErrorInvalidRequest, "The value 1001 for the maxResults parameter must be between 5 and 1000. Change the value and try again."},
		}},
		"DescribeInstanceTypes": {[]int64{5, 100}, []refusal{
			{4, awserrors.ErrorInvalidMaxResults, "Value ( 4 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 100 )."},
			{101, awserrors.ErrorInvalidMaxResults, "Value ( 101 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 100 )."},
		}},
		"DescribeInstanceTypeOfferings": {[]int64{5, 1000}, []refusal{
			{4, awserrors.ErrorInvalidMaxResults, "Value ( 4 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 1000 )."},
			{1001, awserrors.ErrorInvalidMaxResults, "Value ( 1001 ) for parameter maxResults is invalid. Expecting a value from ( 5 ) to  ( 1000 )."},
		}},
		"DescribeLaunchTemplates": {[]int64{1, 200}, []refusal{
			{0, awserrors.ErrorInvalidParameterValue, "Maximum results allowed are between 1 and 200"},
			{201, awserrors.ErrorInvalidParameterValue, "Maximum results allowed are between 1 and 200"},
		}},
		"DescribeNatGateways": {[]int64{5, 1000}, []refusal{
			{4, awserrors.ErrorInvalidParameter, "1 validation error detected: Value '4' at 'maxResults' failed to satisfy constraint: Member must have value greater than or equal to 5"},
			{1001, awserrors.ErrorInvalidParameter, "1 validation error detected: Value '1001' at 'maxResults' failed to satisfy constraint: Member must have value less than or equal to 1000"},
		}},
		"DescribePrincipalIdFormat": {[]int64{1, 1000}, []refusal{
			{0, awserrors.ErrorInvalidParameterValue, "Value ( 0 ) for parameter maxResults is invalid. Expecting a value between 1 and 1000."},
			{1001, awserrors.ErrorInvalidParameterValue, "Value ( 1001 ) for parameter maxResults is invalid. Expecting a value between 1 and 1000."},
		}},
	}
	require.Len(t, cases, len(ec2MaxResultsRanges), "every ranged action needs a case")

	withMaxResults := func(action string, n int64) any {
		handler, ok := ec2Actions[action]
		require.True(t, ok, action)
		input, err := handler.parse(map[string]string{"Action": action})
		require.NoError(t, err, action)
		reflect.ValueOf(input).Elem().FieldByName("MaxResults").Set(reflect.ValueOf(aws.Int64(n)))
		return input
	}
	for action, c := range cases {
		require.Contains(t, ec2MaxResultsRanges, action)
		for _, n := range c.accept {
			assert.NoError(t, validateMaxResults(action, withMaxResults(action, n)), "%s MaxResults %d", action, n)
		}
		for _, r := range c.refuse {
			testutil.RequireAWSError(t, validateMaxResults(action, withMaxResults(action, r.n)), r.code, r.msg)
		}
	}
}
