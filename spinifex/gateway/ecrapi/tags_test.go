package gateway_ecrapi

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const tagsTestRegion = "us-east-1"

func tagResourceARN(repo string) string {
	return "arn:aws:ecr:" + tagsTestRegion + ":" + policyTestAccount + ":repository/" + repo
}

// TestListTagsForResource_ReturnsEmpty now seeds a real repository: it
// resolves the resourceArn against the caller's account rather than
// answering every call with an unconditional empty set.
func TestListTagsForResource_ReturnsEmpty(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "demo")

	out, err := ListTagsForResource(context.Background(), nc, policyTestAccount,
		[]byte(`{"resourceArn":"`+tagResourceARN("demo")+`"}`))
	require.NoError(t, err)

	resp, ok := out.(*ecr.ListTagsForResourceOutput)
	require.True(t, ok, "expected *ecr.ListTagsForResourceOutput")
	assert.NotNil(t, resp.Tags)
	assert.Empty(t, resp.Tags)
}

// TestListTagsForResource_RegisteredNotStub now supplies a resourceArn: the
// real handler needs an identifier to resolve, unlike the old empty stub.
func TestListTagsForResource_RegisteredNotStub(t *testing.T) {
	h, ok := Actions["ListTagsForResource"]
	require.True(t, ok)
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "demo")

	_, err := h(context.Background(), nc, policyTestAccount, []byte(`{"resourceArn":"`+tagResourceARN("demo")+`"}`))
	assert.NoError(t, err, "ListTagsForResource should not resolve to the 501 stub")
}

// TestTagResource_DecodesRealAWSWireFormat proves the decode against the wire
// shape AWS clients actually send: ecr.Tag has no locationName, so its JSON
// keys are Key/Value (capitalized), unlike other fields in this package.
func TestTagResource_DecodesRealAWSWireFormat(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")

	body := []byte(`{"resourceArn":"` + tagResourceARN("team/app") + `","tags":[{"Key":"env","Value":"prod"},{"Key":"team","Value":"platform"}]}`)
	out, err := TagResource(context.Background(), nc, policyTestAccount, body)
	require.NoError(t, err)
	_, ok := out.(*ecr.TagResourceOutput)
	require.True(t, ok)

	listed, err := ListTagsForResource(context.Background(), nc, policyTestAccount,
		[]byte(`{"resourceArn":"`+tagResourceARN("team/app")+`"}`))
	require.NoError(t, err)
	res, ok := listed.(*ecr.ListTagsForResourceOutput)
	require.True(t, ok)
	require.Len(t, res.Tags, 2)
	assert.Equal(t, "env", *res.Tags[0].Key)
	assert.Equal(t, "prod", *res.Tags[0].Value)
	assert.Equal(t, "team", *res.Tags[1].Key)
	assert.Equal(t, "platform", *res.Tags[1].Value)
}

func TestTagResource_MergeAndOverwrite(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")
	arn := tagResourceARN("team/app")

	_, err := TagResource(context.Background(), nc, policyTestAccount,
		[]byte(`{"resourceArn":"`+arn+`","tags":[{"Key":"env","Value":"dev"}]}`))
	require.NoError(t, err)

	// A second call overwrites the existing key and adds a new one.
	_, err = TagResource(context.Background(), nc, policyTestAccount,
		[]byte(`{"resourceArn":"`+arn+`","tags":[{"Key":"env","Value":"prod"},{"Key":"owner","Value":"team"}]}`))
	require.NoError(t, err)

	out, err := ListTagsForResource(context.Background(), nc, policyTestAccount, []byte(`{"resourceArn":"`+arn+`"}`))
	require.NoError(t, err)
	res := out.(*ecr.ListTagsForResourceOutput)
	require.Len(t, res.Tags, 2)
	got := map[string]string{}
	for _, tag := range res.Tags {
		got[*tag.Key] = *tag.Value
	}
	assert.Equal(t, "prod", got["env"])
	assert.Equal(t, "team", got["owner"])
}

func TestUntagResource_RemovesKeysAndIsIdempotent(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")
	arn := tagResourceARN("team/app")

	_, err := TagResource(context.Background(), nc, policyTestAccount,
		[]byte(`{"resourceArn":"`+arn+`","tags":[{"Key":"env","Value":"prod"},{"Key":"owner","Value":"team"}]}`))
	require.NoError(t, err)

	_, err = UntagResource(context.Background(), nc, policyTestAccount,
		[]byte(`{"resourceArn":"`+arn+`","tagKeys":["env","missing"]}`))
	require.NoError(t, err)

	out, err := ListTagsForResource(context.Background(), nc, policyTestAccount, []byte(`{"resourceArn":"`+arn+`"}`))
	require.NoError(t, err)
	res := out.(*ecr.ListTagsForResourceOutput)
	require.Len(t, res.Tags, 1)
	assert.Equal(t, "owner", *res.Tags[0].Key)
}

func TestTagResource_Errors(t *testing.T) {
	nc := newPolicyTestConn(t)
	seedRepo(t, nc, "team/app")

	t.Run("no tags", func(t *testing.T) {
		_, err := TagResource(context.Background(), nc, policyTestAccount,
			[]byte(`{"resourceArn":"`+tagResourceARN("team/app")+`","tags":[]}`))
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	})

	t.Run("malformed arn", func(t *testing.T) {
		_, err := TagResource(context.Background(), nc, policyTestAccount,
			[]byte(`{"resourceArn":"not-an-arn","tags":[{"Key":"env","Value":"prod"}]}`))
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	})

	t.Run("repo not found", func(t *testing.T) {
		_, err := TagResource(context.Background(), nc, policyTestAccount,
			[]byte(`{"resourceArn":"`+tagResourceARN("team/ghost")+`","tags":[{"Key":"env","Value":"prod"}]}`))
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorRepositoryNotFound, err.Error())
	})

	t.Run("empty key rejected", func(t *testing.T) {
		_, err := TagResource(context.Background(), nc, policyTestAccount,
			[]byte(`{"resourceArn":"`+tagResourceARN("team/app")+`","tags":[{"Key":"","Value":"prod"}]}`))
		require.Error(t, err)
		assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
	})
}

func TestUntagResource_Errors(t *testing.T) {
	nc := newPolicyTestConn(t)

	_, err := UntagResource(context.Background(), nc, policyTestAccount,
		[]byte(`{"resourceArn":"`+tagResourceARN("team/app")+`","tagKeys":[]}`))
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}
