package handlers_iam

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sdkTag(key, value string) *iam.Tag {
	return &iam.Tag{Key: aws.String(key), Value: aws.String(value)}
}

func TestValidateTags(t *testing.T) {
	t.Parallel()
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
		{"every allowed symbol", []*iam.Tag{sdkTag("ok_.:/=+-@ key", "ok_.:/=+-@ value")}, ""},
		{"non-ASCII letters", []*iam.Tag{sdkTag("ключ", "значение")}, ""},
		{"max key length", []*iam.Tag{sdkTag(strings.Repeat("k", maxTagKeyLength), "v")}, ""},
		{"max value length", []*iam.Tag{sdkTag("k", strings.Repeat("v", maxTagValueLength))}, ""},
		{"max key length in multibyte characters", []*iam.Tag{sdkTag(strings.Repeat("é", maxTagKeyLength), "v")}, ""},
		{"max value length in multibyte characters", []*iam.Tag{sdkTag("k", strings.Repeat("é", maxTagValueLength))}, ""},
		{"over 50 tags", tooMany, awserrors.ErrorValidationError},
		{"reserved aws: prefix", []*iam.Tag{sdkTag("aws:thing", "v")}, awserrors.ErrorIAMInvalidInput},
		{"reserved prefix in any case", []*iam.Tag{sdkTag("AWS:thing", "v")}, awserrors.ErrorIAMInvalidInput},
		{"aws prefix without colon", []*iam.Tag{sdkTag("awsthing", "v")}, ""},
		{"constraint break wins over reserved prefix", []*iam.Tag{sdkTag("aws:thing", "v"), sdkTag("bad#", "v")}, awserrors.ErrorValidationError},
		{"nil tag entry", []*iam.Tag{nil}, awserrors.ErrorValidationError},
		{"nil key", []*iam.Tag{{Value: aws.String("v")}}, awserrors.ErrorValidationError},
		{"nil value", []*iam.Tag{{Key: aws.String("k")}}, awserrors.ErrorValidationError},
		{"empty key", []*iam.Tag{sdkTag("", "v")}, awserrors.ErrorValidationError},
		{"over-length key", []*iam.Tag{sdkTag(strings.Repeat("é", maxTagKeyLength+1), "v")}, awserrors.ErrorValidationError},
		{"over-length value", []*iam.Tag{sdkTag("k", strings.Repeat("é", maxTagValueLength+1))}, awserrors.ErrorValidationError},
		{"key outside pattern", []*iam.Tag{sdkTag("bad#key", "v")}, awserrors.ErrorValidationError},
		{"value outside pattern", []*iam.Tag{sdkTag("k", "bad\nvalue")}, awserrors.ErrorValidationError},
		{"duplicate keys", []*iam.Tag{sdkTag("k", "1"), sdkTag("k", "2")}, awserrors.ErrorIAMInvalidInput},
		{"duplicate keys differing in case", []*iam.Tag{sdkTag("Dup", "1"), sdkTag("dup", "2")}, awserrors.ErrorIAMInvalidInput},
		{"constraint break wins over duplicate", []*iam.Tag{sdkTag("k", "1"), sdkTag("K", "2"), sdkTag("bad#", "v")}, awserrors.ErrorValidationError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateTags(tc.tags, foldedKeys)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			code, ok := awserrors.ResolveErrorCode(err)
			require.True(t, ok, "error must carry a registered code: %v", err)
			assert.Equal(t, tc.wantErr, code)
		})
	}
}

// The messages are the ones AWS returned to TagUser for the same input.
func TestValidateTags_Messages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tags     []*iam.Tag
		wantCode string
		wantMsg  string
	}{
		{"nil value", []*iam.Tag{{Key: aws.String("novalue")}}, awserrors.ErrorValidationError,
			"1 validation error detected: Value at 'tags.1.member.value' failed to satisfy constraint: Member must not be null"},
		{"key outside pattern", []*iam.Tag{sdkTag("bad#key", "v")}, awserrors.ErrorValidationError,
			`1 validation error detected: Value at 'tags.1.member.key' failed to satisfy constraint: Member must satisfy regular expression pattern: [\p{L}\p{Z}\p{N}_.:/=+\-@]+`},
		{"value outside pattern", []*iam.Tag{sdkTag("k", "bad#value")}, awserrors.ErrorValidationError,
			`1 validation error detected: Value at 'tags.1.member.value' failed to satisfy constraint: Member must satisfy regular expression pattern: [\p{L}\p{Z}\p{N}_.:/=+\-@]*`},
		{"over-length key", []*iam.Tag{sdkTag(strings.Repeat("é", maxTagKeyLength+1), "v")}, awserrors.ErrorValidationError,
			"1 validation error detected: Value at 'tags.1.member.key' failed to satisfy constraint: Member must have length less than or equal to 128"},
		{"over-length value", []*iam.Tag{sdkTag("k", strings.Repeat("é", maxTagValueLength+1))}, awserrors.ErrorValidationError,
			"1 validation error detected: Value at 'tags.1.member.value' failed to satisfy constraint: Member must have length less than or equal to 256"},
		{"empty key breaks length and pattern", []*iam.Tag{sdkTag("", "v")}, awserrors.ErrorValidationError,
			"2 validation errors detected: Value at 'tags.1.member.key' failed to satisfy constraint: Member must have length greater than or equal to 1; " +
				`Value at 'tags.1.member.key' failed to satisfy constraint: Member must satisfy regular expression pattern: [\p{L}\p{Z}\p{N}_.:/=+\-@]+`},
		{"names the failing member", []*iam.Tag{sdkTag("a", "1"), {Value: aws.String("v")}}, awserrors.ErrorValidationError,
			"1 validation error detected: Value at 'tags.2.member.key' failed to satisfy constraint: Member must not be null"},
		{"duplicate keys differing in case", []*iam.Tag{sdkTag("Dup", "1"), sdkTag("dup", "2")}, awserrors.ErrorIAMInvalidInput,
			"Duplicate tag keys found. Please note that Tag keys are case insensitive."},
		{"reserved aws: prefix", []*iam.Tag{sdkTag("aws:thing", "v")}, awserrors.ErrorIAMInvalidInput,
			"Tag keys beginning with aws: are reserved for system use."},
		{"over 50 tags with a bad key", append(tagsNamed(maxTagsPerResource), sdkTag("bad#key", "v")), awserrors.ErrorValidationError,
			"2 validation errors detected: Value at 'tags' failed to satisfy constraint: Member must have length less than or equal to 50; " +
				`Value at 'tags.51.member.key' failed to satisfy constraint: Member must satisfy regular expression pattern: [\p{L}\p{Z}\p{N}_.:/=+\-@]+`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			requireIAMError(t, validateTags(tc.tags, foldedKeys), tc.wantCode, tc.wantMsg)
		})
	}
}

func TestValidateTagKeys(t *testing.T) {
	t.Parallel()
	keySet := `Value at 'tagKeys' failed to satisfy constraint: Member must satisfy constraint: [Member must have length less than or equal to 128, ` +
		`Member must have length greater than or equal to 1, Member must satisfy regular expression pattern: [\p{L}\p{Z}\p{N}_.:/=+\-@]+, Member must not be null]`
	tooMany := make([]*string, maxTagsPerResource+1)
	for i := range tooMany {
		tooMany[i] = aws.String(fmt.Sprintf("k%d", i))
	}
	cases := []struct {
		name    string
		keys    []*string
		wantMsg string
	}{
		{"valid keys", []*string{aws.String("env"), aws.String(strings.Repeat("é", maxTagKeyLength))}, ""},
		{"empty key", []*string{aws.String("")}, "1 validation error detected: " + keySet},
		{"key outside pattern", []*string{aws.String("bad#key")}, "1 validation error detected: " + keySet},
		{"over-length key", []*string{aws.String(strings.Repeat("k", maxTagKeyLength+1))}, "1 validation error detected: " + keySet},
		{"several bad keys are one violation", []*string{aws.String("bad#"), aws.String("k"), aws.String("bad*")}, "1 validation error detected: " + keySet},
		{"nil key", []*string{nil}, "1 validation error detected: " + keySet},
		{"over 50 keys", tooMany,
			"1 validation error detected: Value at 'tagKeys' failed to satisfy constraint: Member must have length less than or equal to 50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateTagKeys(tc.keys)
			if tc.wantMsg == "" {
				require.NoError(t, err)
				return
			}
			requireIAMError(t, err, awserrors.ErrorValidationError, tc.wantMsg)
		})
	}
}

// AWS checks the keys before looking the resource up.
func TestUntag_InvalidKeyRefusedBeforeLookup(t *testing.T) {
	t.Parallel()
	for _, ops := range allTagOps() {
		t.Run(ops.resource, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			err := ops.untag(svc, ops.missingID, []*string{aws.String("bad#key")})
			code, ok := awserrors.ResolveErrorCode(err)
			require.True(t, ok, "error must carry a registered code: %v", err)
			assert.Equal(t, awserrors.ErrorValidationError, code)
		})
	}
}

func tagsNamed(n int) []*iam.Tag {
	tags := make([]*iam.Tag, n)
	for i := range tags {
		tags[i] = sdkTag(fmt.Sprintf("k%d", i), "v")
	}
	return tags
}

func TestMergeTags(t *testing.T) {
	t.Parallel()
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
			"upsert ignores case and takes the new key",
			[]Tag{{Key: "Env", Value: "base"}, {Key: "b", Value: "2"}},
			[]*iam.Tag{sdkTag("env", "lower")},
			[]Tag{{Key: "env", Value: "lower"}, {Key: "b", Value: "2"}},
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
			t.Parallel()
			got := mergeTags(tc.existing, tc.add, foldedKeys)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMergeAndRemoveTags_ExactKeys(t *testing.T) {
	t.Parallel()
	existing := []Tag{{Key: "Env", Value: "base"}}
	merged := mergeTags(existing, []*iam.Tag{sdkTag("env", "lower")}, exactKeys)
	assert.Equal(t, []Tag{{Key: "Env", Value: "base"}, {Key: "env", Value: "lower"}}, merged)
	assert.Equal(t, merged, removeTagKeys(merged, []*string{aws.String("ENV")}, exactKeys))
	assert.Equal(t, []Tag{{Key: "env", Value: "lower"}}, removeTagKeys(merged, []*string{aws.String("Env")}, exactKeys))
	require.NoError(t, validateTags([]*iam.Tag{sdkTag("Dup", "1"), sdkTag("dup", "2")}, exactKeys))
	requireIAMError(t, validateTags([]*iam.Tag{sdkTag("dup", "1"), sdkTag("dup", "2")}, exactKeys), awserrors.ErrorIAMInvalidInput,
		"Duplicate tag keys found. Please note that Tag keys are case insensitive.")
}

func TestMergeTags_DoesNotMutateExisting(t *testing.T) {
	t.Parallel()
	existing := []Tag{{Key: "a", Value: "1"}}
	_ = mergeTags(existing, []*iam.Tag{sdkTag("a", "9")}, foldedKeys)
	assert.Equal(t, []Tag{{Key: "a", Value: "1"}}, existing)
}

func TestRemoveTagKeys(t *testing.T) {
	t.Parallel()
	existing := []Tag{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}}

	cases := []struct {
		name string
		keys []*string
		want []Tag
	}{
		{"remove one", []*string{aws.String("b")}, []Tag{{Key: "a", Value: "1"}, {Key: "c", Value: "3"}}},
		{"key case ignored", []*string{aws.String("B")}, []Tag{{Key: "a", Value: "1"}, {Key: "c", Value: "3"}}},
		{"unknown key ignored", []*string{aws.String("zzz")}, existing},
		{"nil key ignored", []*string{nil, aws.String("a")}, []Tag{{Key: "b", Value: "2"}, {Key: "c", Value: "3"}}},
		{"remove all", []*string{aws.String("a"), aws.String("b"), aws.String("c")}, []Tag{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := removeTagKeys(existing, tc.keys, foldedKeys)
			assert.Equal(t, tc.want, got)
		})
	}
}

// tagOps abstracts the per-resource tag/untag/list triple so the round-trip
// behaviour is asserted identically for every taggable resource.
type tagOps struct {
	resource string
	keys     keyCase
	create   func(t *testing.T, svc *IAMServiceImpl) string
	// createTagged creates the resource with tags and returns the ID it would have.
	createTagged func(svc *IAMServiceImpl, tags []*iam.Tag) (string, error)
	missingID    string
	tag          func(svc *IAMServiceImpl, id string, tags []*iam.Tag) error
	untag        func(svc *IAMServiceImpl, id string, keys []*string) error
	list         func(svc *IAMServiceImpl, id string) ([]*iam.Tag, error)
}

func allTagOps() []tagOps {
	return []tagOps{
		{
			resource: "user",
			keys:     foldedKeys,
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				return *createTestUser(t, svc, "tagme").UserName
			},
			createTagged: func(svc *IAMServiceImpl, tags []*iam.Tag) (string, error) {
				_, err := svc.CreateUser(testAccountID, &iam.CreateUserInput{UserName: aws.String("tagged"), Tags: tags})
				return "tagged", err
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
			keys:     foldedKeys,
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				return *createTestRole(t, svc, "tagme").RoleName
			},
			createTagged: func(svc *IAMServiceImpl, tags []*iam.Tag) (string, error) {
				_, err := svc.CreateRole(testAccountID, &iam.CreateRoleInput{
					RoleName: aws.String("tagged"), AssumeRolePolicyDocument: aws.String(validTrustPolicy()), Tags: tags,
				})
				return "tagged", err
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
			keys:     exactKeys,
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				out, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
					PolicyName:     aws.String("tagme"),
					PolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`),
				})
				require.NoError(t, err)
				return *out.Policy.Arn
			},
			createTagged: func(svc *IAMServiceImpl, tags []*iam.Tag) (string, error) {
				_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
					PolicyName: aws.String("tagged"), PolicyDocument: aws.String(validPolicyDocument()), Tags: tags,
				})
				return "arn:aws:iam::" + testAccountID + ":policy/tagged", err
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
			keys:     exactKeys,
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				return *createTestInstanceProfile(t, svc, "tagme").InstanceProfileName
			},
			createTagged: func(svc *IAMServiceImpl, tags []*iam.Tag) (string, error) {
				_, err := svc.CreateInstanceProfile(testAccountID, &iam.CreateInstanceProfileInput{
					InstanceProfileName: aws.String("tagged"), Tags: tags,
				})
				return "tagged", err
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
			keys:     exactKeys,
			create: func(t *testing.T, svc *IAMServiceImpl) string {
				out, err := svc.CreateOpenIDConnectProvider(testAccountID, &iam.CreateOpenIDConnectProviderInput{
					Url: aws.String("https://oidc.example.com/id/TAGME"),
				})
				require.NoError(t, err)
				return *out.OpenIDConnectProviderArn
			},
			createTagged: func(svc *IAMServiceImpl, tags []*iam.Tag) (string, error) {
				_, err := svc.CreateOpenIDConnectProvider(testAccountID, &iam.CreateOpenIDConnectProviderInput{
					Url: aws.String("https://oidc.example.com/id/TAGGED"), Tags: tags,
				})
				return "arn:aws:iam::" + testAccountID + ":oidc-provider/oidc.example.com/id/TAGGED", err
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

func TestCreate_InvalidTagsRefusedAndNotStored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tags     []*iam.Tag
		wantCode string
	}{
		{"nil value", []*iam.Tag{{Key: aws.String("k")}}, awserrors.ErrorValidationError},
		{"key outside pattern", []*iam.Tag{sdkTag("bad#key", "v")}, awserrors.ErrorValidationError},
		{"duplicate keys", []*iam.Tag{sdkTag("dup", "1"), sdkTag("dup", "2")}, awserrors.ErrorIAMInvalidInput},
		{"reserved aws: prefix", []*iam.Tag{sdkTag("aws:thing", "v")}, awserrors.ErrorIAMInvalidInput},
	}
	for _, ops := range allTagOps() {
		for _, tc := range cases {
			t.Run(ops.resource+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				svc := setupTestIAMService(t)
				id, err := ops.createTagged(svc, tc.tags)
				require.Error(t, err)
				code, ok := awserrors.ResolveErrorCode(err)
				require.True(t, ok, "error must carry a registered code: %v", err)
				assert.Equal(t, tc.wantCode, code)

				_, err = ops.list(svc, id)
				require.Error(t, err)
				assert.Equal(t, awserrors.ErrorIAMNoSuchEntity, err.Error())
			})
		}
	}
}

func TestCreate_ValidTagsStored(t *testing.T) {
	t.Parallel()
	for _, ops := range allTagOps() {
		t.Run(ops.resource, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			id, err := ops.createTagged(svc, []*iam.Tag{sdkTag("env", "prod"), sdkTag("team", "")})
			require.NoError(t, err)
			got, err := ops.list(svc, id)
			require.NoError(t, err)
			assert.Equal(t, map[string]string{"env": "prod", "team": ""}, tagsAsMap(got))
		})
	}
}

// AWS compares tag keys ignoring case on users and roles only: elsewhere Dup
// and dup are two tags, and neither tagging nor untagging in another case
// touches the stored key.
func TestResourceTagging_KeyCase(t *testing.T) {
	t.Parallel()
	for _, ops := range allTagOps() {
		t.Run(ops.resource, func(t *testing.T) {
			t.Parallel()
			svc := setupTestIAMService(t)
			id := ops.create(t, svc)

			err := ops.tag(svc, id, []*iam.Tag{sdkTag("Dup", "1"), sdkTag("dup", "2")})
			if ops.keys == foldedKeys {
				requireIAMError(t, err, awserrors.ErrorIAMInvalidInput, "Duplicate tag keys found. Please note that Tag keys are case insensitive.")
			} else {
				require.NoError(t, err)
			}

			require.NoError(t, ops.tag(svc, id, []*iam.Tag{sdkTag("Case", "upper")}))
			require.NoError(t, ops.tag(svc, id, []*iam.Tag{sdkTag("case", "lower")}))
			got, err := ops.list(svc, id)
			require.NoError(t, err)
			want := map[string]string{"Dup": "1", "dup": "2", "Case": "upper", "case": "lower"}
			if ops.keys == foldedKeys {
				want = map[string]string{"case": "lower"}
			}
			assert.Equal(t, want, tagsAsMap(got))

			require.NoError(t, ops.untag(svc, id, []*string{aws.String("CASE")}))
			got, err = ops.list(svc, id)
			require.NoError(t, err)
			if ops.keys == foldedKeys {
				want = map[string]string{}
			}
			assert.Equal(t, want, tagsAsMap(got))
		})
	}
}

func TestResourceTagging_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, ops := range allTagOps() {
		t.Run(ops.resource, func(t *testing.T) {
			t.Parallel()
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

func TestTagUser_GetUserSurfacesTags(t *testing.T) {
	t.Parallel()
	svc := setupTestIAMService(t)
	user := createTestUser(t, svc, "tagme")

	_, err := svc.TagUser(testAccountID, &iam.TagUserInput{
		UserName: user.UserName,
		Tags:     []*iam.Tag{sdkTag("env", "prod")},
	})
	require.NoError(t, err)

	out, err := svc.GetUser(testAccountID, &iam.GetUserInput{UserName: user.UserName})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"env": "prod"}, tagsAsMap(out.User.Tags))
}

func TestResourceTagging_MissingEntity(t *testing.T) {
	t.Parallel()
	for _, ops := range allTagOps() {
		t.Run(ops.resource, func(t *testing.T) {
			t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	assert.Nil(t, tagsToSDK(nil))
	assert.Nil(t, tagsToSDK([]Tag{}))
	got := tagsToSDK([]Tag{{Key: "a", Value: "1"}})
	require.Len(t, got, 1)
	assert.Equal(t, "a", aws.StringValue(got[0].Key))
	assert.Equal(t, "1", aws.StringValue(got[0].Value))
}
