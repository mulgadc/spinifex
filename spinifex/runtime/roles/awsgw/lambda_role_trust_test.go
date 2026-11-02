package awsgw

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The trust decision is the stored role's real trust policy evaluated for
// lambda.amazonaws.com, so only a role Lambda could assume is accepted.
func TestLambdaRoleTrusts_EvaluatesTheStoredTrustPolicy(t *testing.T) {
	iamSvc, _ := newCodeStoreIAM(t)
	acct := identifiers.GlobalAccountID
	trusts := lambdaRoleTrusts(iamSvc)

	allowLambda := `{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}`
	cases := []struct {
		name, statements string
		want             bool
	}{
		{"trusts lambda", allowLambda, true},
		{"trusts only ec2", `{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}`, false},
		{"explicit deny wins", allowLambda + `,{"Effect":"Deny","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}`, false},
		{"source account matches", `{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"` + acct + `"}}}`, true},
		{"source account differs", `{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole","Condition":{"StringEquals":{"aws:SourceAccount":"123456789012"}}}`, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "exec-" + string(rune('a'+i))
			out, err := iamSvc.CreateRole(acct, &iam.CreateRoleInput{
				RoleName:                 aws.String(name),
				AssumeRolePolicyDocument: aws.String(`{"Version":"2012-10-17","Statement":[` + tc.statements + `]}`),
			})
			require.NoError(t, err)
			got, err := trusts(acct, aws.StringValue(out.Role.Arn))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("missing role is not trusted", func(t *testing.T) {
		got, err := trusts(acct, "arn:aws:iam::"+acct+":role/absent")
		require.NoError(t, err)
		assert.False(t, got)
	})
}
