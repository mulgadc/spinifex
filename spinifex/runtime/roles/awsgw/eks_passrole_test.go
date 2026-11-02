package awsgw_test

import (
	"errors"
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

const eksNodeRoleARN = "arn:aws:iam::" + authzAccountID + ":role/eks/node-role"

const foreignAccountID = "999999999999"

// roleStoreIAMService evaluates policy from docs and resolves role names to the
// ARNs in roles, keyed by account then name, the way the IAM store does.
type roleStoreIAMService struct {
	gwfix.PolicyIAM

	roles        map[string]map[string]string
	canonicalErr error
}

func (s *roleStoreIAMService) CanonicalResourceARN(account string, _ arn.IAMResourceType, name string) (string, error) {
	if s.canonicalErr != nil {
		return "", s.canonicalErr
	}
	if roleARN, ok := s.roles[account][name]; ok {
		return roleARN, nil
	}
	return "", errors.New(awserrors.ErrorIAMNoSuchEntity)
}
