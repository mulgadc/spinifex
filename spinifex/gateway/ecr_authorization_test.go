package gateway

import (
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
)

// ecrAllowPolicy and ecrDenyPolicy build single-statement policy documents for
// the ECR operation-authorization tests below. They are distinct from
// auth_test.go's allowPolicy/allowPolicyResource helpers because those are
// fixed to a single action string; ECR route classification can require
// multiple actions per operation (e.g. cross-repo mount).
func ecrAllowPolicy(actions, resources []string) iamdomain.PolicyDocument {
	return iamdomain.PolicyDocument{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: actions, Resource: resources},
		},
	}
}

func ecrDenyPolicy(actions, resources []string) iamdomain.PolicyDocument {
	return iamdomain.PolicyDocument{
		Version: "2012-10-17",
		Statement: []iamdomain.Statement{
			{Effect: "Deny", Action: actions, Resource: resources},
		},
	}
}
