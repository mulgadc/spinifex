package handlers_iam

import (
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

func TestTagsToSDK(t *testing.T) {
	assert.Nil(t, tagsToSDK(nil))
	assert.Nil(t, tagsToSDK([]Tag{}))
	got := tagsToSDK([]Tag{{Key: "a", Value: "1"}})
	require.Len(t, got, 1)
	assert.Equal(t, "a", aws.StringValue(got[0].Key))
	assert.Equal(t, "1", aws.StringValue(got[0].Value))
}
