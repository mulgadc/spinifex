package iam

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/service/iam"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetachPolicyARN_NotAttachedLeavesListAlone(t *testing.T) {
	t.Parallel()
	attached := []string{"arn:a", "arn:b"}
	err := detachPolicyARN(&attached, "arn:c")
	requireIAMError(t, err, awserrors.ErrorIAMNoSuchEntity, "Policy arn:c was not found.")
	assert.Equal(t, []string{"arn:a", "arn:b"}, attached)

	require.NoError(t, detachPolicyARN(&attached, "arn:a"))
	assert.Equal(t, []string{"arn:b"}, attached)
}

func TestPutInlinePolicy_OverQuotaLeavesPoliciesAlone(t *testing.T) {
	t.Parallel()
	var inline map[string]string
	require.NoError(t, putInlinePolicy(&inline, "user", "alice", userPolicySizeQuota, "small", `{"a":1}`))
	assert.Equal(t, map[string]string{"small": `{"a":1}`}, inline)

	big := `"` + strings.Repeat("x", userPolicySizeQuota) + `"`
	err := putInlinePolicy(&inline, "user", "alice", userPolicySizeQuota, "big", big)
	requireIAMError(t, err, awserrors.ErrorIAMLimitExceeded,
		fmt.Sprintf("Maximum policy size of %d bytes exceeded for user alice", userPolicySizeQuota))
	assert.Equal(t, map[string]string{"small": `{"a":1}`}, inline)
}

func TestApplyTags_OverLimitLeavesTagsAlone(t *testing.T) {
	t.Parallel()
	tags := make([]Tag, maxTagsPerResource)
	for i := range tags {
		tags[i] = Tag{Key: fmt.Sprintf("k%d", i)}
	}
	before := append([]Tag(nil), tags...)

	err := applyTags(&tags, []*iam.Tag{sdkTag("extra", "v")}, exactKeys)
	code, _ := awserrors.ResolveErrorCode(err)
	assert.Equal(t, awserrors.ErrorIAMLimitExceeded, code)
	assert.Equal(t, before, tags)

	require.NoError(t, applyTags(&tags, []*iam.Tag{sdkTag("K0", "new")}, foldedKeys))
	assert.Equal(t, Tag{Key: "K0", Value: "new"}, tags[0])
	assert.Len(t, tags, maxTagsPerResource)
}
