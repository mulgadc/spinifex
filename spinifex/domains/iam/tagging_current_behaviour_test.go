package iam

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The TestCurrentBehaviour_Tagging_ tests pin how IAM treats tag-based
// authorization today. They describe current behaviour, not the target design,
// and a failure means the behaviour changed.

// A customer-managed policy conditioned on any tag key family is refused at
// write time, so no stored policy can carry a tag condition the evaluator
// would then have to enforce.
func TestCurrentBehaviour_Tagging_CreatePolicyRefusesTagConditionKeys(t *testing.T) {
	svc := setupTestIAMService(t)

	conditions := map[string]struct{ key, condition string }{
		"ResourceTag": {"aws:ResourceTag/team", `{"StringEquals":{"aws:ResourceTag/team":"infra"}}`},
		"RequestTag":  {"aws:RequestTag/team", `{"StringEquals":{"aws:RequestTag/team":"infra"}}`},
		"TagKeys":     {"aws:TagKeys", `{"ForAllValues:StringEquals":{"aws:TagKeys":["team"]}}`},
		"EC2Resource": {"ec2:ResourceTag/team", `{"StringEquals":{"ec2:ResourceTag/team":"infra"}}`},
	}
	for name, tc := range conditions {
		t.Run(name, func(t *testing.T) {
			doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:CreateTags","Resource":"*","Condition":` + tc.condition + `}]}`
			_, err := svc.CreatePolicy(testAccountID, &iam.CreatePolicyInput{
				PolicyName:     aws.String("cb-tag-" + name),
				PolicyDocument: aws.String(doc),
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorIAMMalformedPolicyDocument)
			assert.Contains(t, err.Error(), `key "`+tc.key+`" is not supported`)
		})
	}
}
