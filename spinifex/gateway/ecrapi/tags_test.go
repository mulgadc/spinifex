package gateway_ecrapi

import (
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListTagsForResource_ReturnsEmpty(t *testing.T) {
	out, err := ListTagsForResource(nil, "123456789012", []byte(`{"resourceArn":"arn:aws:ecr:ap-southeast-2:123456789012:repository/demo"}`))
	require.NoError(t, err)

	resp, ok := out.(*ecr.ListTagsForResourceOutput)
	require.True(t, ok, "expected *ecr.ListTagsForResourceOutput")
	assert.NotNil(t, resp.Tags)
	assert.Empty(t, resp.Tags)
}

func TestListTagsForResource_RegisteredNotStub(t *testing.T) {
	h, ok := Actions["ListTagsForResource"]
	require.True(t, ok)
	_, err := h(nil, "123456789012", []byte("{}"))
	assert.NoError(t, err, "ListTagsForResource should not resolve to the 501 stub")
}
