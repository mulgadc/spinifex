package sts

import (
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testIRSAIssuer  = "https://oidc.eks.au-mel-1.amazonaws.com/id/ABCDEF0123456789"
	testIRSAFedARN  = "arn:aws:iam::000000000001:oidc-provider/oidc.eks.au-mel-1.amazonaws.com/id/ABCDEF0123456789"
	testIRSASubject = "system:serviceaccount:kube-system:aws-node"
)

func irsaIdentity() webIdentityContext {
	return webIdentityContext{
		federatedPrincipalARN: testIRSAFedARN,
		issuer:                testIRSAIssuer,
		subject:               testIRSASubject,
		audience:              []string{irsaExpectedAudience},
	}
}

// irsaPolicy builds an Allow trust policy with one Condition block.
func irsaPolicy(condition string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity","Condition":%s}]}`,
		testIRSAFedARN, condition)
}

func TestEvalWebIdentity_ConditionKeys(t *testing.T) {
	awsKey := strings.TrimPrefix(testIRSAIssuer, "https://")
	legacy := testIRSAIssuer

	tests := []struct {
		name      string
		condition string
		grant     bool
	}{
		{
			name:      "AWS-form sub+aud grants",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q,%q:%q}}`, awsKey+":sub", testIRSASubject, awsKey+":aud", irsaExpectedAudience),
			grant:     true,
		},
		{
			name:      "AWS-form mismatched sub denies",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q,%q:%q}}`, awsKey+":sub", "system:serviceaccount:default:other", awsKey+":aud", irsaExpectedAudience),
		},
		{
			name:      "AWS-form mismatched aud denies",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q,%q:%q}}`, awsKey+":sub", testIRSASubject, awsKey+":aud", "other.example.com"),
		},
		{
			name:      "AWS-form aud array grants when any entry matches",
			condition: fmt.Sprintf(`{"StringEquals":{%q:[%q,%q]}}`, awsKey+":aud", "other.example.com", irsaExpectedAudience),
			grant:     true,
		},
		{
			name:      "legacy https:// form still grants",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q,%q:%q}}`, legacy+":sub", testIRSASubject, legacy+":aud", irsaExpectedAudience),
			grant:     true,
		},
		{
			name:      "StringLike sub wildcard grants",
			condition: fmt.Sprintf(`{"StringLike":{%q:%q}}`, awsKey+":sub", "system:serviceaccount:kube-system:*"),
			grant:     true,
		},
		{
			name:      "StringLike sub single-char wildcard grants",
			condition: fmt.Sprintf(`{"StringLike":{%q:%q}}`, awsKey+":sub", "system:serviceaccount:kube-system:aws-nod?"),
			grant:     true,
		},
		{
			name:      "StringLike sub non-matching pattern denies",
			condition: fmt.Sprintf(`{"StringLike":{%q:%q}}`, awsKey+":sub", "system:serviceaccount:default:*"),
		},
		{
			name:      "StringLike is case-sensitive",
			condition: fmt.Sprintf(`{"StringLike":{%q:%q}}`, awsKey+":sub", "SYSTEM:serviceaccount:kube-system:*"),
		},
		{
			name:      "StringLike on legacy-form key grants",
			condition: fmt.Sprintf(`{"StringLike":{%q:%q}}`, legacy+":sub", "system:serviceaccount:kube-system:*"),
			grant:     true,
		},
		{
			name: "StringEquals aud + StringLike sub grants when both hold",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q},"StringLike":{%q:%q}}`,
				awsKey+":aud", irsaExpectedAudience, awsKey+":sub", "system:serviceaccount:kube-system:*"),
			grant: true,
		},
		{
			name: "StringEquals aud + StringLike sub denies when aud fails",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q},"StringLike":{%q:%q}}`,
				awsKey+":aud", "other.example.com", awsKey+":sub", "system:serviceaccount:kube-system:*"),
		},
		{
			name: "StringEquals aud + StringLike sub denies when sub fails",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q},"StringLike":{%q:%q}}`,
				awsKey+":aud", irsaExpectedAudience, awsKey+":sub", "system:serviceaccount:default:*"),
		},
		{
			name:      "unknown key denies",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q}}`, awsKey+":email", "a@example.com"),
		},
		{
			name:      "key for a different issuer denies",
			condition: fmt.Sprintf(`{"StringEquals":{%q:%q}}`, "oidc.eks.au-mel-1.amazonaws.com/id/OTHER:sub", testIRSASubject),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := evalTrustPolicyForWebIdentity(irsaPolicy(tt.condition), irsaIdentity())
			if tt.grant {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
		})
	}
}

// An explicit Deny with a StringLike condition on the AWS-form key must still win over an Allow.
func TestEvalWebIdentity_ExplicitDenyAWSFormStringLike(t *testing.T) {
	awsKey := strings.TrimPrefix(testIRSAIssuer, "https://")
	policy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity"},
		{"Effect":"Deny","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{"StringLike":{%q:%q}}}
	]}`, testIRSAFedARN, testIRSAFedARN, awsKey+":sub", "system:serviceaccount:kube-system:*")

	err := evalTrustPolicyForWebIdentity(policy, irsaIdentity())
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

// The stock Terraform IRSA trust policy keys on the issuer without its scheme.
func TestAssumeRoleWithWebIdentity_AWSFormConditionKeys(t *testing.T) {
	svc, _ := newTestSetup(t)
	f := newWebIdentityFixture(t, svc, testCallerAccountID)
	prefix := strings.TrimPrefix(f.issuer, "https://")

	policyFor := func(sub string) string {
		return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Federated":%q},"Action":"sts:AssumeRoleWithWebIdentity","Condition":{"StringEquals":{%q:%q,%q:%q}}}]}`,
			f.federatedARN, prefix+":sub", sub, prefix+":aud", irsaExpectedAudience)
	}
	assume := func(roleArn *string) error {
		_, err := svc.AssumeRoleWithWebIdentity(&sts.AssumeRoleWithWebIdentityInput{
			RoleArn:          roleArn,
			RoleSessionName:  aws.String("sess"),
			WebIdentityToken: aws.String(f.signToken(f.defaultClaims())),
		})
		return err
	}

	granted := createRoleForFixture(t, svc, testCallerAccountID, "irsa-aws-form-ok", policyFor("system:serviceaccount:default:my-sa"))
	require.NoError(t, assume(granted.Arn))

	denied := createRoleForFixture(t, svc, testCallerAccountID, "irsa-aws-form-bad", policyFor("system:serviceaccount:default:other-sa"))
	err := assume(denied.Arn)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}
