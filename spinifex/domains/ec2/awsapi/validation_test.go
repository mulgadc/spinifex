//test:in-package — walks the unexported ec2Actions route table.

package awsapi

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
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

// Each paged action refuses a MaxResults outside its AWS range before it pages.
func TestValidateMaxResults_PagedActionRanges(t *testing.T) {
	const ltText = "Maximum results allowed are between 1 and 200"
	cases := []struct {
		action string
		input  func(n int64) any
		min    int64
		max    int64
		low    string
		high   string
	}{
		{"DescribeLaunchTemplates", func(n int64) any { return &ec2.DescribeLaunchTemplatesInput{MaxResults: aws.Int64(n)} },
			1, 200, ltText, ltText},
		{"DescribeLaunchTemplateVersions", func(n int64) any { return &ec2.DescribeLaunchTemplateVersionsInput{MaxResults: aws.Int64(n)} },
			1, 200, ltText, ltText},
		{"DescribeCapacityReservations", func(n int64) any { return &ec2.DescribeCapacityReservationsInput{MaxResults: aws.Int64(n)} },
			1, 1000,
			"0 is an invalid value for MaxResults. MaxResults must be null, or between 1 and 1000.",
			"1001 is an invalid value for MaxResults. MaxResults must be null, or between 1 and 1000."},
		{"DescribeAddressesAttribute", func(n int64) any { return &ec2.DescribeAddressesAttributeInput{MaxResults: aws.Int64(n)} },
			1, 1000,
			"1 validation error detected: Value '0' at 'maxResults' failed to satisfy constraint: Member must have value greater than or equal to 1",
			"1 validation error detected: Value '1001' at 'maxResults' failed to satisfy constraint: Member must have value less than or equal to 1000"},
	}
	for _, c := range cases {
		t.Run(c.action, func(t *testing.T) {
			require.NoError(t, validateMaxResults(c.action, c.input(c.min)))
			require.NoError(t, validateMaxResults(c.action, c.input(c.max)))
			testutil.RequireAWSError(t, validateMaxResults(c.action, c.input(c.min-1)), awserrors.ErrorInvalidParameterValue, c.low)
			testutil.RequireAWSError(t, validateMaxResults(c.action, c.input(c.max+1)), awserrors.ErrorInvalidParameterValue, c.high)
		})
	}
}
