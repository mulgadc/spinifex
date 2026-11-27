package awserrors

import (
	"regexp"
	"strings"
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var htmlTag = regexp.MustCompile(`<[^>]+>`)

// The one EKS not-found code covers clusters, node groups, Fargate profiles
// and addons, so its default message must be the model's resource-neutral text.
func TestErrorLookup_EKSResourceNotFoundMatchesModel(t *testing.T) {
	model, err := awsmodel.Load(awsmodel.EKS)
	require.NoError(t, err)
	shape, ok := model.Shape(ErrorEKSResourceNotFound)
	require.True(t, ok)
	require.NotNil(t, shape.Error)
	documented := strings.Join(strings.Fields(htmlTag.ReplaceAllString(shape.Documentation, "")), " ")
	require.NotEmpty(t, documented)

	entry := ErrorLookup[ErrorEKSResourceNotFound]
	assert.Equal(t, documented, entry.Message)
	assert.Equal(t, shape.Error.HTTPStatusCode, entry.HTTPCode)
	assert.Equal(t, 404, entry.HTTPCode)
}

// RegisterTargets declares both target limits, and each code keeps the
// model's status. The aggregate code also keeps the model's message.
func TestErrorLookup_ELBv2TargetLimitsMatchModel(t *testing.T) {
	model, err := awsmodel.Load(awsmodel.ElasticLoadBalancingV2)
	require.NoError(t, err)
	for shapeName, code := range map[string]string{
		"TooManyTargetsException":                  ErrorELBv2TooManyTargetsAggregate,
		"TooManyRegistrationsForTargetIdException": ErrorELBv2TooManyTargets,
	} {
		shape, ok := model.Shape(shapeName)
		require.True(t, ok, shapeName)
		require.NotNil(t, shape.Error, shapeName)
		assert.Equal(t, shape.Error.Code, code)
		assert.Equal(t, shape.Error.HTTPStatusCode, ErrorLookup[code].HTTPCode, code)
		body := []byte(`<ErrorResponse><Error><Code>` + code + `</Code></Error></ErrorResponse>`)
		violations, modelled, err := awsmodel.ValidateErrorResponse(awsmodel.ElasticLoadBalancingV2, "RegisterTargets", body)
		require.NoError(t, err)
		assert.True(t, modelled, code)
		assert.Empty(t, violations, code)
	}
	shape, _ := model.Shape("TooManyTargetsException")
	documented := strings.Join(strings.Fields(htmlTag.ReplaceAllString(shape.Documentation, "")), " ")
	assert.Equal(t, documented, ErrorLookup[ErrorELBv2TooManyTargetsAggregate].Message)
}
