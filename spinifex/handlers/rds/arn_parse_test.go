package handlers_rds

import (
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every ARN the builders mint must parse back into the parts it was built
// from, so a resource named by a describe is addressable by a tag action.
func TestParseARN_RoundTripsEveryResourceType(t *testing.T) {
	automated := AutomatedSnapshotIdentifier("orders-db", time.Date(2026, 7, 24, 3, 4, 0, 0, time.UTC))
	cases := []struct {
		name       string
		kind       ResourceKind
		identifier string
		arn        string
	}{
		{"instance", ResourceKindDBInstance, "orders-db", FormatARN(ResourceKindDBInstance, testRegion, testAccountID, "orders-db")},
		{"manual snapshot", ResourceKindDBSnapshot, "orders-db-2026-07-24", FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, "orders-db-2026-07-24")},
		{"automated snapshot", ResourceKindDBSnapshot, automated, FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, automated)},
		{"subnet group", ResourceKindDBSubnetGroup, "prod-db-subnets", FormatARN(ResourceKindDBSubnetGroup, testRegion, testAccountID, "prod-db-subnets")},
		{"parameter group", ResourceKindDBParameterGroup, "postgres16-tuned", FormatARN(ResourceKindDBParameterGroup, testRegion, testAccountID, "postgres16-tuned")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := ParseARN(tc.arn, testRegion, testAccountID)
			require.NoError(t, err)
			assert.Equal(t, tc.kind, parsed.Kind)
			assert.Equal(t, tc.identifier, parsed.Identifier)
			assert.Equal(t, testRegion, parsed.Region)
			assert.Equal(t, testAccountID, parsed.AccountID)
		})
	}
}

func TestParseARN_RejectsMalformedAndForeignARNs(t *testing.T) {
	cases := []struct {
		name string
		arn  string
	}{
		// The ECS shape. Copying its parser would accept this and read the whole
		// "db/orders-db" as the resource type.
		{"slash delimited", "arn:aws:rds:" + testRegion + ":" + testAccountID + ":db/orders-db"},
		{"truncated", "arn:aws:rds:" + testRegion + ":" + testAccountID + ":db"},
		{"extra segment", FormatARN(ResourceKindDBInstance, testRegion, testAccountID, "orders-db") + ":extra"},
		{"snapshot with a foreign namespace", FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, "other:orders-db")},
		{"snapshot with two embedded colons", FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, "rds:orders-db:extra")},
		{"snapshot with a slash", FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, "rds:orders/db")},
		{"empty identifier", "arn:aws:rds:" + testRegion + ":" + testAccountID + ":db:"},
		{"another service", "arn:aws:ec2:" + testRegion + ":" + testAccountID + ":db:orders-db"},
		{"another partition", "arn:aws-cn:rds:" + testRegion + ":" + testAccountID + ":db:orders-db"},
		{"unknown resource type", "arn:aws:rds:" + testRegion + ":" + testAccountID + ":cluster:orders"},
		// Rejected at the ARN rather than at policy evaluation, so a foreign
		// reference never reaches the evaluator at all.
		{"foreign account", FormatARN(ResourceKindDBInstance, testRegion, "210987654321", "orders-db")},
		{"foreign region", FormatARN(ResourceKindDBInstance, "us-east-1", testAccountID, "orders-db")},
		{"empty", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseARN(tc.arn, testRegion, testAccountID)
			require.Error(t, err)
			assert.Contains(t, err.Error(), awserrors.ErrorInvalidParameterValue)
		})
	}
}

// The registry is what makes an unregistered kind a rejection rather than an
// empty tag list, so every kind the parser accepts has to be accounted for.
func TestTaggableResources_CoverEveryResourceThatExists(t *testing.T) {
	t.Parallel()
	for _, kind := range []ResourceKind{ResourceKindDBInstance, ResourceKindDBSnapshot,
		ResourceKindDBSubnetGroup, ResourceKindDBParameterGroup} {
		_, ok := taggableResources[kind]
		assert.True(t, ok, "%s is a resource that exists and so must be taggable", kind)
	}
}
