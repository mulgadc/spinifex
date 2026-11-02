package iam

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpdateUser_RenameKeepsRelationships(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	before := createTestUser(t, svc, "bob")
	policy := createTestPolicy(t, svc, "bob-managed")
	createTestGroup(t, svc, "devs")

	_, err := svc.AttachUserPolicy(testAccountID, &iam.AttachUserPolicyInput{UserName: aws.String("bob"), PolicyArn: policy.Arn})
	require.NoError(t, err)
	_, err = svc.PutUserPolicy(testAccountID, &iam.PutUserPolicyInput{
		UserName: aws.String("bob"), PolicyName: aws.String("inline"), PolicyDocument: aws.String(validPolicyDocument()),
	})
	require.NoError(t, err)
	_, err = svc.AddUserToGroup(testAccountID, &iam.AddUserToGroupInput{UserName: aws.String("bob"), GroupName: aws.String("devs")})
	require.NoError(t, err)
	key, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{UserName: aws.String("bob")})
	require.NoError(t, err)

	out, err := svc.UpdateUser(testAccountID, &iam.UpdateUserInput{
		UserName: aws.String("bob"), NewUserName: aws.String("robert"), NewPath: aws.String("/eng/"),
	})
	require.NoError(t, err)
	assert.Equal(t, &iam.UpdateUserOutput{}, out)

	_, err = svc.GetUser(testAccountID, &iam.GetUserInput{UserName: aws.String("bob")})
	require.Error(t, err)
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity), err)

	got, err := svc.GetUser(testAccountID, &iam.GetUserInput{UserName: aws.String("robert")})
	require.NoError(t, err)
	assert.Equal(t, *before.UserId, *got.User.UserId)
	assert.Equal(t, "/eng/", *got.User.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":user/eng/robert", *got.User.Arn)

	ak, err := svc.LookupAccessKey(*key.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.Equal(t, "robert", ak.UserName)
	keys, err := svc.ListAccessKeys(testAccountID, &iam.ListAccessKeysInput{UserName: aws.String("robert")})
	require.NoError(t, err)
	require.Len(t, keys.AccessKeyMetadata, 1)
	assert.Equal(t, *key.AccessKey.AccessKeyId, *keys.AccessKeyMetadata[0].AccessKeyId)

	attached, err := svc.ListAttachedUserPolicies(testAccountID, &iam.ListAttachedUserPoliciesInput{UserName: aws.String("robert")})
	require.NoError(t, err)
	require.Len(t, attached.AttachedPolicies, 1)
	assert.Equal(t, *policy.Arn, *attached.AttachedPolicies[0].PolicyArn)

	inline, err := svc.ListUserPolicies(testAccountID, &iam.ListUserPoliciesInput{UserName: aws.String("robert")})
	require.NoError(t, err)
	assert.Equal(t, []*string{aws.String("inline")}, inline.PolicyNames)

	groups, err := svc.ListGroupsForUser(testAccountID, &iam.ListGroupsForUserInput{UserName: aws.String("robert")})
	require.NoError(t, err)
	require.Len(t, groups.Groups, 1)
	assert.Equal(t, "devs", *groups.Groups[0].GroupName)

	members, err := svc.GetGroup(testAccountID, &iam.GetGroupInput{GroupName: aws.String("devs")})
	require.NoError(t, err)
	require.Len(t, members.Users, 1)
	assert.Equal(t, "robert", *members.Users[0].UserName)
}

func TestUpdateUser_PathOnly(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "carol")

	_, err := svc.UpdateUser(testAccountID, &iam.UpdateUserInput{UserName: aws.String("carol"), NewPath: aws.String("/ops/")})
	require.NoError(t, err)

	got, err := svc.GetUser(testAccountID, &iam.GetUserInput{UserName: aws.String("carol")})
	require.NoError(t, err)
	assert.Equal(t, "/ops/", *got.User.Path)
	assert.Equal(t, "arn:aws:iam::"+testAccountID+":user/ops/carol", *got.User.Arn)
}

func TestUpdateUser_Errors(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "dave")
	createTestUser(t, svc, "erin")

	tests := []struct {
		name  string
		input *iam.UpdateUserInput
		want  string
	}{
		{"missing user", &iam.UpdateUserInput{UserName: aws.String("ghost"), NewPath: aws.String("/x/")}, awserrors.ErrorIAMNoSuchEntity},
		{"missing user rename", &iam.UpdateUserInput{UserName: aws.String("ghost"), NewUserName: aws.String("ghost2")}, awserrors.ErrorIAMNoSuchEntity},
		{"taken name", &iam.UpdateUserInput{UserName: aws.String("dave"), NewUserName: aws.String("erin")}, awserrors.ErrorIAMEntityAlreadyExists},
		{"bad name", &iam.UpdateUserInput{UserName: aws.String("dave"), NewUserName: aws.String("no spaces")}, awserrors.ErrorValidationError},
		{"bad path", &iam.UpdateUserInput{UserName: aws.String("dave"), NewPath: aws.String("noslash")}, awserrors.ErrorValidationError},
	}
	for _, tc := range tests {
		_, err := svc.UpdateUser(testAccountID, tc.input)
		require.Error(t, err, tc.name)
		assert.True(t, awserrors.IsErrorCode(err, tc.want), "%s: got %v want %s", tc.name, err, tc.want)
	}

	got, err := svc.GetUser(testAccountID, &iam.GetUserInput{UserName: aws.String("dave")})
	require.NoError(t, err)
	assert.Equal(t, "/", *got.User.Path, "a refused update leaves the user unchanged")
}

// racingUsersKV writes the old user record once, right after the renamed
// record is created, as a concurrent AttachUserPolicy on another node would.
type racingUsersKV struct {
	jetstream.KeyValue

	oldKey string
	raced  bool
}

func (k *racingUsersKV) Create(ctx context.Context, key string, value []byte, opts ...jetstream.KVCreateOpt) (uint64, error) {
	rev, err := k.KeyValue.Create(ctx, key, value, opts...)
	if err != nil || k.raced {
		return rev, err
	}
	k.raced = true
	entry, gerr := k.Get(ctx, k.oldKey)
	if gerr != nil {
		return rev, gerr
	}
	var u User
	if uerr := json.Unmarshal(entry.Value(), &u); uerr != nil {
		return rev, uerr
	}
	u.AttachedPolicies = append(u.AttachedPolicies, "arn:aws:iam::"+testAccountID+":policy/raced")
	data, _ := json.Marshal(u)
	if _, perr := k.Update(ctx, k.oldKey, data, entry.Revision()); perr != nil {
		return rev, perr
	}
	return rev, nil
}

func TestUpdateUser_RenameRetriesAfterRacingWrite(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "frank")
	key, err := svc.CreateAccessKey(testAccountID, &iam.CreateAccessKeyInput{UserName: aws.String("frank")})
	require.NoError(t, err)
	svc.usersBucket = &racingUsersKV{KeyValue: svc.usersBucket, oldKey: testAccountID + ".frank"}

	_, err = svc.UpdateUser(testAccountID, &iam.UpdateUserInput{UserName: aws.String("frank"), NewUserName: aws.String("franklin")})
	require.NoError(t, err)

	user, err := svc.getUser(t.Context(), testAccountID, "franklin")
	require.NoError(t, err)
	assert.Equal(t, []string{"arn:aws:iam::" + testAccountID + ":policy/raced"}, user.AttachedPolicies,
		"the racing write survives the rename")
	_, err = svc.getUser(t.Context(), testAccountID, "frank")
	assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorIAMNoSuchEntity), err)
	ak, err := svc.LookupAccessKey(*key.AccessKey.AccessKeyId)
	require.NoError(t, err)
	assert.Equal(t, "franklin", ak.UserName)
}
