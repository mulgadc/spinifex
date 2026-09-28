//go:build integration

package integration

import (
	"testing"

	"github.com/mulgadc/spinifex/internal/awsmodel"
	"github.com/stretchr/testify/require"
)

func TestRequestConformanceBlocksOnlyRequestPromotedServices(t *testing.T) {
	collector := newRequestCollector()
	accepted := awsmodel.RequestCase{Operation: "ListRoles", Constraint: awsmodel.ConstraintRange, Path: "$.MaxItems", Detail: "above max 1000"}
	collector.record(awsmodel.IAM, accepted, awsmodel.RequestResult{Status: 200}, awsmodel.VerdictFinding)

	responseOnly := conformancePolicy{promoted: map[awsmodel.Service]bool{awsmodel.IAM: true}}
	require.Zero(t, collector.blocking(responseOnly, conformanceModeFail), "response promotion does not promote requests")

	requestPromoted := conformancePolicy{requestPromoted: map[awsmodel.Service]bool{awsmodel.IAM: true}}
	require.Equal(t, 1, collector.blocking(requestPromoted, conformanceModeFail))
	require.Zero(t, collector.blocking(requestPromoted, conformanceModeWarn))
	require.Contains(t, collector.report(requestPromoted, conformanceModeFail), "FAIL iam ListRoles $.MaxItems range (above max 1000) accepted")
}

func TestRequestConformanceReportsUndeclaredErrorsAndUntestedOperations(t *testing.T) {
	collector := newRequestCollector()
	refused := awsmodel.RequestCase{Operation: "TagResource", Constraint: awsmodel.ConstraintLength, Path: "$.tags.{key}", Detail: "below min 1"}
	collector.record(awsmodel.ECR, refused, awsmodel.RequestResult{Status: 400, Code: "InvalidParameterValueException", Message: "bad key"}, awsmodel.VerdictUndeclaredError)
	collector.recordOperation(awsmodel.ACM, "RequestCertificate", awsmodel.RequestPlan{Skipped: []awsmodel.Skip{{Path: "$.DomainName", Reason: "pattern is not supported by Go regexp"}}})

	policy := conformancePolicy{requestPromoted: map[awsmodel.Service]bool{awsmodel.ECR: true}}
	require.Equal(t, 1, collector.blocking(policy, conformanceModeFail), "an undeclared error blocks like any finding")
	report := collector.report(policy, conformanceModeFail)
	require.Contains(t, report, "FAIL ecr TagResource $.tags.{key} length (below min 1) refused with undeclared InvalidParameterValueException: bad key")
	require.Contains(t, report, "UNTESTED acm RequestCertificate: $.DomainName: pattern is not supported by Go regexp")
	require.Contains(t, report, "operations=1 untested_operations=1 requests=0")
}
