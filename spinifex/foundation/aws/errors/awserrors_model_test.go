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
