package gateway_iam_test

import (
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	gateway_iam "github.com/mulgadc/spinifex/spinifex/gateway/iam"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAccountID = "000000000000"

// listStub serves fixed lists in whatever order the test gives, as the KV
// bucket does, and counts service calls.
type listStub struct {
	handlers_iam.IAMService

	users    []string
	versions []string
	entities func() *iam.ListEntitiesForPolicyOutput
	calls    int
}

func (s *listStub) ListUsers(_ string, _ *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	s.calls++
	users := make([]*iam.User, 0, len(s.users))
	for _, name := range s.users {
		users = append(users, &iam.User{UserName: aws.String(name)})
	}
	return &iam.ListUsersOutput{Users: users, IsTruncated: aws.Bool(false)}, nil
}

func (s *listStub) ListPolicyVersions(_ string, _ *iam.ListPolicyVersionsInput) (*iam.ListPolicyVersionsOutput, error) {
	versions := make([]*iam.PolicyVersion, 0, len(s.versions))
	for _, id := range s.versions {
		versions = append(versions, &iam.PolicyVersion{VersionId: aws.String(id)})
	}
	return &iam.ListPolicyVersionsOutput{Versions: versions, IsTruncated: aws.Bool(false)}, nil
}

func (s *listStub) ListEntitiesForPolicy(_ string, _ *iam.ListEntitiesForPolicyInput) (*iam.ListEntitiesForPolicyOutput, error) {
	return s.entities(), nil
}

func userNames(users []*iam.User) []string {
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, aws.StringValue(u.UserName))
	}
	return names
}

func listUsers(t *testing.T, svc *listStub, marker *string, maxItems int64) *iam.ListUsersOutput {
	t.Helper()
	out, err := gateway_iam.ListUsers(testAccountID, &iam.ListUsersInput{Marker: marker, MaxItems: aws.Int64(maxItems)}, svc)
	require.NoError(t, err)
	return out
}

func TestListUsers_MaxItemsTruncatesAndMarkerResumes(t *testing.T) {
	svc := &listStub{users: []string{"carol", "alice", "bob"}}

	first := listUsers(t, svc, nil, 2)
	assert.Equal(t, []string{"alice", "bob"}, userNames(first.Users))
	assert.True(t, aws.BoolValue(first.IsTruncated))
	require.NotNil(t, first.Marker)

	second := listUsers(t, svc, first.Marker, 2)
	assert.Equal(t, []string{"carol"}, userNames(second.Users))
	assert.False(t, aws.BoolValue(second.IsTruncated))
	assert.Nil(t, second.Marker)
}

func TestListUsers_DefaultPageIs100(t *testing.T) {
	svc := &listStub{}
	for i := range 101 {
		svc.users = append(svc.users, fmt.Sprintf("user%03d", i))
	}

	out, err := gateway_iam.ListUsers(testAccountID, &iam.ListUsersInput{}, svc)
	require.NoError(t, err)
	assert.Len(t, out.Users, 100)
	assert.True(t, aws.BoolValue(out.IsTruncated))
	require.NotNil(t, out.Marker)
}

func TestListUsers_ExactFitIsNotTruncated(t *testing.T) {
	svc := &listStub{users: []string{"a", "b"}}

	out := listUsers(t, svc, nil, 2)
	assert.Len(t, out.Users, 2)
	assert.False(t, aws.BoolValue(out.IsTruncated))
	assert.Nil(t, out.Marker)
}

// The marker names the last user returned, so the next page neither repeats nor
// skips a user when the list changes around the boundary between calls.
func TestListUsers_MarkerSurvivesChangesBetweenPages(t *testing.T) {
	svc := &listStub{users: []string{"a", "b", "c", "d"}}
	first := listUsers(t, svc, nil, 2)
	require.Equal(t, []string{"a", "b"}, userNames(first.Users))

	svc.users = []string{"d", "c", "aa", "a"}
	second := listUsers(t, svc, first.Marker, 2)
	assert.Equal(t, []string{"c", "d"}, userNames(second.Users))
	assert.False(t, aws.BoolValue(second.IsTruncated))
}

func TestListUsers_RejectsBadPagingBeforeListing(t *testing.T) {
	tests := []struct {
		name  string
		input *iam.ListUsersInput
	}{
		{"MaxItems zero", &iam.ListUsersInput{MaxItems: aws.Int64(0)}},
		{"MaxItems above 1000", &iam.ListUsersInput{MaxItems: aws.Int64(1001)}},
		{"empty Marker", &iam.ListUsersInput{Marker: aws.String("")}},
		{"undecodable Marker", &iam.ListUsersInput{Marker: aws.String("not a marker!")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &listStub{users: []string{"a"}}
			_, err := gateway_iam.ListUsers(testAccountID, tc.input, svc)
			require.Error(t, err)
			assert.True(t, awserrors.IsErrorCode(err, awserrors.ErrorValidationError), "got %v", err)
			assert.Zero(t, svc.calls)
		})
	}
}

func TestListUsers_MaxItemsBoundsAccepted(t *testing.T) {
	svc := &listStub{users: []string{"a", "b"}}
	assert.Len(t, listUsers(t, svc, nil, 1).Users, 1)
	assert.Len(t, listUsers(t, svc, nil, 1000).Users, 2)
}

func TestListPolicyVersions_PagesNewestFirst(t *testing.T) {
	svc := &listStub{versions: []string{"v2", "v10", "v9"}}
	ids := func(out *iam.ListPolicyVersionsOutput) []string {
		var got []string
		for _, v := range out.Versions {
			got = append(got, aws.StringValue(v.VersionId))
		}
		return got
	}

	first, err := gateway_iam.ListPolicyVersions(testAccountID, &iam.ListPolicyVersionsInput{PolicyArn: aws.String("arn"), MaxItems: aws.Int64(2)}, svc)
	require.NoError(t, err)
	assert.Equal(t, []string{"v10", "v9"}, ids(first))
	assert.True(t, aws.BoolValue(first.IsTruncated))

	second, err := gateway_iam.ListPolicyVersions(testAccountID, &iam.ListPolicyVersionsInput{PolicyArn: aws.String("arn"), MaxItems: aws.Int64(2), Marker: first.Marker}, svc)
	require.NoError(t, err)
	assert.Equal(t, []string{"v2"}, ids(second))
	assert.False(t, aws.BoolValue(second.IsTruncated))
}

// Groups, roles and users share one MaxItems, so a page can end partway
// through one list and the next page picks up in it.
func TestListEntitiesForPolicy_PagesAcrossAllThreeLists(t *testing.T) {
	svc := &listStub{entities: func() *iam.ListEntitiesForPolicyOutput {
		return &iam.ListEntitiesForPolicyOutput{
			PolicyUsers:  []*iam.PolicyUser{{UserName: aws.String("u1")}},
			PolicyRoles:  []*iam.PolicyRole{{RoleName: aws.String("r2")}, {RoleName: aws.String("r1")}},
			PolicyGroups: []*iam.PolicyGroup{{GroupName: aws.String("g1")}},
		}
	}}
	input := &iam.ListEntitiesForPolicyInput{PolicyArn: aws.String("arn"), MaxItems: aws.Int64(2)}

	first, err := gateway_iam.ListEntitiesForPolicy(testAccountID, input, svc)
	require.NoError(t, err)
	require.Len(t, first.PolicyGroups, 1)
	require.Len(t, first.PolicyRoles, 1)
	assert.Equal(t, "r1", aws.StringValue(first.PolicyRoles[0].RoleName))
	assert.Empty(t, first.PolicyUsers)
	assert.NotNil(t, first.PolicyUsers)
	assert.True(t, aws.BoolValue(first.IsTruncated))

	input.Marker = first.Marker
	second, err := gateway_iam.ListEntitiesForPolicy(testAccountID, input, svc)
	require.NoError(t, err)
	assert.Empty(t, second.PolicyGroups)
	require.Len(t, second.PolicyRoles, 1)
	assert.Equal(t, "r2", aws.StringValue(second.PolicyRoles[0].RoleName))
	require.Len(t, second.PolicyUsers, 1)
	assert.False(t, aws.BoolValue(second.IsTruncated))
	assert.Nil(t, second.Marker)
}
