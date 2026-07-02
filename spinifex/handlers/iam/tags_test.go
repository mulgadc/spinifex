package handlers_iam

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sdkTag(key, value string) *iam.Tag {
	return &iam.Tag{Key: aws.String(key), Value: aws.String(value)}
}

func TestValidateTags(t *testing.T) {
	tooMany := make([]*iam.Tag, maxTagsPerResource+1)
	for i := range tooMany {
		tooMany[i] = sdkTag("key"+strings.Repeat("a", i%100+1), "v")
	}

	cases := []struct {
		name    string
		tags    []*iam.Tag
		wantErr string
	}{
		{"nil slice", nil, ""},
		{"valid tags", []*iam.Tag{sdkTag("env", "prod"), sdkTag("team", "")}, ""},
		{"max key length", []*iam.Tag{sdkTag(strings.Repeat("k", maxTagKeyLength), "v")}, ""},
		{"max value length", []*iam.Tag{sdkTag("k", strings.Repeat("v", maxTagValueLength))}, ""},
		{"nil value allowed", []*iam.Tag{{Key: aws.String("k")}}, ""},
		{"over 50 tags", tooMany, awserrors.ErrorIAMLimitExceeded},
		{"nil tag entry", []*iam.Tag{nil}, awserrors.ErrorIAMInvalidInput},
		{"nil key", []*iam.Tag{{Value: aws.String("v")}}, awserrors.ErrorIAMInvalidInput},
		{"empty key", []*iam.Tag{sdkTag("", "v")}, awserrors.ErrorIAMInvalidInput},
		{"over-length key", []*iam.Tag{sdkTag(strings.Repeat("k", maxTagKeyLength+1), "v")}, awserrors.ErrorIAMInvalidInput},
		{"over-length value", []*iam.Tag{sdkTag("k", strings.Repeat("v", maxTagValueLength+1))}, awserrors.ErrorIAMInvalidInput},
		{"duplicate keys", []*iam.Tag{sdkTag("k", "1"), sdkTag("k", "2")}, awserrors.ErrorIAMInvalidInput},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTags(tc.tags)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.wantErr, err.Error())
		})
	}
}

func TestMergeTags(t *testing.T) {
	cases := []struct {
		name     string
		existing []Tag
		add      []*iam.Tag
		want     []Tag
	}{
		{"append to empty", nil, []*iam.Tag{sdkTag("a", "1")}, []Tag{{Key: "a", Value: "1"}}},
		{
			"upsert preserves order",
			[]Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}},
			[]*iam.Tag{sdkTag("a", "9"), sdkTag("c", "3")},
			[]Tag{{Key: "a", Value: "9"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}},
		},
		{
			"nil key skipped",
			[]Tag{{Key: "a", Value: "1"}},
			[]*iam.Tag{{Value: aws.String("v")}},
			[]Tag{{Key: "a", Value: "1"}},
		},
		{
			"nil value stored empty",
			nil,
			[]*iam.Tag{{Key: aws.String("a")}},
			[]Tag{{Key: "a", Value: ""}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeTags(tc.existing, tc.add)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMergeTags_DoesNotMutateExisting(t *testing.T) {
	existing := []Tag{{Key: "a", Value: "1"}}
	_ = mergeTags(existing, []*iam.Tag{sdkTag("a", "9")})
	assert.Equal(t, []Tag{{Key: "a", Value: "1"}}, existing)
}

func TestRemoveTagKeys(t *testing.T) {
	existing := []Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}}

	cases := []struct {
		name string
		keys []*string
		want []Tag
	}{
		{"remove one", []*string{aws.String("b")}, []Tag{{Key: "a", Value: "1"}, {Key: "c", Value: "3"}}},
		{"unknown key ignored", []*string{aws.String("zzz")}, existing},
		{"nil key ignored", []*string{nil, aws.String("a")}, []Tag{{Key: "b", Value: "2"}, {Key: "c", Value: "3"}}},
		{"remove all", []*string{aws.String("a"), aws.String("b"), aws.String("c")}, []Tag{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := removeTagKeys(existing, tc.keys)
			assert.Equal(t, tc.want, got)
		})
	}
}

// tagOps abstracts the per-resource tag/untag/list triple so the round-trip
// behaviour is asserted identically for every taggable resource.
type tagOps struct {
	resource  string
	create    func(t *testing.T, svc *IAMServiceImpl) string
	missingID string
	tag       func(svc *IAMServiceImpl, id string, tags []*iam.Tag) error
	untag     func(svc *IAMServiceImpl, id string, keys []*string) error
	list      func(svc *IAMServiceImpl, id string) ([]*iam.Tag, error)
}

func allTagOps() []tagOps {
	return []tagOps{
		{
			resource: "user",
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				return *createTestUser(t, svc, "tagme").UserName
			},
			missingID: "no-such-user",
			tag: func(svc *IAMServiceImpl, id string, tags []*iam.Tag) error {
				_, err := svc.TagUser(testAccountID, &iam.TagUserInput{UserName: aws.String(id), Tags: tags})
				return err
			},
			untag: func(svc *IAMServiceImpl, id string, keys []*string) error {
				_, err := svc.UntagUser(testAccountID, &iam.UntagUserInput{UserName: aws.String(id), TagKeys: keys})
				return err
			},
			list: func(svc *IAMServiceImpl, id string) ([]*iam.Tag, error) {
				out, err := svc.ListUserTags(testAccountID, &iam.ListUserTagsInput{UserName: aws.String(id)})
				if err != nil {
					return nil, err
				}
				return out.Tags, nil
			},
		},
		{
			resource: "role",
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				return *createTestRole(t, svc, "tagme").RoleName
			},
			missingID: "no-such-role",
			tag: func(svc *IAMServiceImpl, id string, tags []*iam.Tag) error {
				_, err := svc.TagRole(testAccountID, &iam.TagRoleInput{RoleName: aws.String(id), Tags: tags})
				return err
			},
			untag: func(svc *IAMServiceImpl, id string, keys []*string) error {
				_, err := svc.UntagRole(testAccountID, &iam.UntagRoleInput{RoleName: aws.String(id), TagKeys: keys})
				return err
			},
			list: func(svc *IAMServiceImpl, id string) ([]*iam.Tag, error) {
				out, err := svc.ListRoleTags(testAccountID, &iam.ListRoleTagsInput{RoleName: aws.String(id)})
				if err != nil {
					return nil, err
				}
				return out.Tags, nil
			},
		},
		{
			resource: "policy",
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				out, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
					PolicyName:     aws.String("tagme"),
					PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
				})
				require.NoError(t, err)
				return *out.Policy.Arn
			},
			missingID: "arn:aws:iam::" + testAccountID + ":policy/no-such-policy",
			tag: func(svc *IAMServiceImpl, id string, tags []*iam.Tag) error {
				_, err := svc.TagPolicy(testAccountID, &iam.TagPolicyInput{PolicyArn: aws.String(id), Tags: tags})
				return err
			},
			untag: func(svc *IAMServiceImpl, id string, keys []*string) error {
				_, err := svc.UntagPolicy(testAccountID, &iam.UntagPolicyInput{PolicyArn: aws.String(id), TagKeys: keys})
				return err
			},
			list: func(svc *IAMServiceImpl, id string) ([]*iam.Tag, error) {
				out, err := svc.ListPolicyTags(testAccountID, &iam.ListPolicyTagsInput{PolicyArn: aws.String(id)})
				if err != nil {
					return nil, err
				}
				return out.Tags, nil
			},
		},
		{
			resource: "instance profile",
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				return *createTestInstanceProfile(t, svc, "tagme").InstanceProfileName
			},
			missingID: "no-such-profile",
			tag: func(svc *IAMServiceImpl, id string, tags []*iam.Tag) error {
				_, err := svc.TagInstanceProfile(testAccountID, &iam.TagInstanceProfileInput{InstanceProfileName: aws.String(id), Tags: tags})
				return err
			},
			untag: func(svc *IAMServiceImpl, id string, keys []*string) error {
				_, err := svc.UntagInstanceProfile(testAccountID, &iam.UntagInstanceProfileInput{InstanceProfileName: aws.String(id), TagKeys: keys})
				return err
			},
			list: func(svc *IAMServiceImpl, id string) ([]*iam.Tag, error) {
				out, err := svc.ListInstanceProfileTags(testAccountID, &iam.ListInstanceProfileTagsInput{InstanceProfileName: aws.String(id)})
				if err != nil {
					return nil, err
				}
				return out.Tags, nil
			},
		},
		{
			resource: "OIDC provider",
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				out, err := svc.CreateOpenIDConnectProvider(testAccountID, &iam.CreateOpenIDConnectProviderInput{
					Url: aws.String("https://oidc.example.com/id/TAGME"),
				})
				require.NoError(t, err)
				return *out.OpenIDConnectProviderArn
			},
			missingID: "arn:aws:iam::" + testAccountID + ":oidc-provider/oidc.example.com/id/MISSING",
			tag: func(svc *IAMServiceImpl, id string, tags []*iam.Tag) error {
				_, err := svc.TagOpenIDConnectProvider(testAccountID, &iam.TagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(id), Tags: tags})
				return err
			},
			untag: func(svc *IAMServiceImpl, id string, keys []*string) error {
				_, err := svc.UntagOpenIDConnectProvider(testAccountID, &iam.UntagOpenIDConnectProviderInput{OpenIDConnectProviderArn: aws.String(id), TagKeys: keys})
				return err
			},
			list: func(svc *IAMServiceImpl, id string) ([]*iam.Tag, error) {
				out, err := svc.ListOpenIDConnectProviderTags(testAccountID, &iam.ListOpenIDConnectProviderTagsInput{OpenIDConnectProviderArn: aws.String(id)})
				if err != nil {
					return nil, err
				}
				return out.Tags, nil
			},
		},
	}
}

func tagsAsMap(tags []*iam.Tag) map[string]string {
	m := make(map[string]string, len(tags))
	for _, tag := range tags {
		m[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	return m
}

func TestResourceTagging_RoundTrip(t *testing.T) {
	for _, ops := range allTagOps() {
		t.Run(ops.resource, func(t *testing.T) {
			svc := setupTestIAMService(t)
			id := ops.create(t, svc)

			tags, err := ops.list(svc, id)
			require.NoError(t, err)
			assert.Empty(t, tags)

			require.NoError(t, ops.tag(svc, id, []*iam.Tag{sdkTag("env", "prod"), sdkTag("team", "core")}))
			tags, err = ops.list(svc, id)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"env": "prod", "team": "core"}, tagsAsMap(tags))

			// Re-tagging an existing key upserts in place.
			require.NoError(t, ops.tag(svc, id, []*iam.Tag{sdkTag("env", "staging")}))
			tags, err = ops.list(svc, id)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"env": "staging", "team": "core"}, tagsAsMap(tags))

			// Untag removes only the named keys; unknown keys are a no-op.
			require.NoError(t, ops.untag(svc, id, []*string{aws.String("team"), aws.String("unknown")}))
			tags, err = ops.list(svc, id)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"env": "staging"}, tagsAsMap(tags))
		})
	}
}

func TestResourceTagging_MissingEntity(t *testing.T) {
	for _, ops := range allTagOps() {
		t.Run(ops.resource, func(t *testing.T) {
			svc := setupTestIAMService(t)

			err := ops.tag(svc, ops.missingID, []*iam.Tag{sdkTag("env", "prod")})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())

			err = ops.untag(svc, ops.missingID, []*string{aws.String("env")})
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())

			_, err = ops.list(svc, ops.missingID)
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())
		})
	}
}

func TestTagUser_MergedLimitExceeded(t *testing.T) {
	svc := setupTestIAMService(t)
	createTestUser(t, svc, "taglimit")

	fill := make([]*iam.Tag, maxTagsPerResource)
	for i := range fill {
		fill[i] = sdkTag(fmt.Sprintf("key-%02d", i), "v")
	}
	_, err := svc.TagUser(testAccountID, &iam.TagUserInput{UserName: aws.String("taglimit"), Tags: fill})
	require.NoError(t, err)

	// One more distinct key would push the merged set past the limit.
	_, err = svc.TagUser(testAccountID, &iam.TagUserInput{
		UserName: aws.String("taglimit"),
		Tags:     []*iam.Tag{sdkTag("overflow", "v")},
	})
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIAMLimitExceeded, err.Error())

	// Upserting an existing key does not grow the set and stays allowed.
	_, err = svc.TagUser(testAccountID, &iam.TagUserInput{
		UserName: aws.String("taglimit"),
		Tags:     []*iam.Tag{sdkTag("key-00", "updated")},
	})
	require.NoError(t, err)
}

func TestTagsToSDK(t *testing.T) {
	assert.Nil(t, tagsToSDK(nil))
	assert.Nil(t, tagsToSDK([]Tag{}))
	got := tagsToSDK([]Tag{{Key: "a", Value: "1"}})
	require.Len(t, got, 1)
	assert.Equal(t, "a", aws.StringValue(got[0].Key))
	assert.Equal(t, "1", aws.StringValue(got[0].Value))
}
