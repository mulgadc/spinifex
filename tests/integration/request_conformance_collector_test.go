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
