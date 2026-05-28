package handlers_sts

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSTSServiceImpl_GetCallerIdentity_IAMUser(t *testing.T) {
	svc, _ := newTestSetup(t)
	out, err := svc.GetCallerIdentity(
		"000000000000",
		"arn:aws:iam::000000000000:user/alice",
		"AIDAEXAMPLEAAAAAAAAA",
		&sts.GetCallerIdentityInput{},
	)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "000000000000", aws.StringValue(out.Account))
	assert.Equal(t, "arn:aws:iam::000000000000:user/alice", aws.StringValue(out.Arn))
	assert.Equal(t, "AIDAEXAMPLEAAAAAAAAA", aws.StringValue(out.UserId))
}

func TestSTSServiceImpl_GetCallerIdentity_AssumedRole(t *testing.T) {
	svc, _ := newTestSetup(t)
	out, err := svc.GetCallerIdentity(
		"000000000000",
		"arn:aws:sts::000000000000:assumed-role/app-role/sess-1",
		"AROAEXAMPLEAAAAAAAAA:sess-1",
		&sts.GetCallerIdentityInput{},
	)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "000000000000", aws.StringValue(out.Account))
	assert.Equal(t, "arn:aws:sts::000000000000:assumed-role/app-role/sess-1", aws.StringValue(out.Arn))
	assert.Equal(t, "AROAEXAMPLEAAAAAAAAA:sess-1", aws.StringValue(out.UserId))
}

func TestSTSServiceImpl_GetCallerIdentity_Root(t *testing.T) {
	svc, _ := newTestSetup(t)
	out, err := svc.GetCallerIdentity(
		"000000000000",
		"arn:aws:iam::000000000000:root",
		"000000000000",
		&sts.GetCallerIdentityInput{},
	)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "000000000000", aws.StringValue(out.Account))
	assert.Equal(t, "arn:aws:iam::000000000000:root", aws.StringValue(out.Arn))
	assert.Equal(t, "000000000000", aws.StringValue(out.UserId))
}

func TestSTSServiceImpl_GetCallerIdentity_NilInput(t *testing.T) {
	svc, _ := newTestSetup(t)
	out, err := svc.GetCallerIdentity(
		"000000000000",
		"arn:aws:iam::000000000000:user/alice",
		"AIDAEXAMPLEAAAAAAAAA",
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "arn:aws:iam::000000000000:user/alice", aws.StringValue(out.Arn))
}
