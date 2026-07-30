package handlers_rds

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every D17 ARN the builders mint must parse back into the parts it was built
// from, so a resource named by a describe is addressable by a tag action.
func TestParseARN_RoundTripsEveryResourceType(t *testing.T) {
	cases := []struct {
		kind       ResourceKind
		identifier string
		arn        string
	}{
		{ResourceKindDBInstance, "orders-db", DBInstanceARN(testRegion, testAccountID, "orders-db")},
		{ResourceKindDBSnapshot, "orders-db-2026-07-24", DBSnapshotARN(testRegion, testAccountID, "orders-db-2026-07-24")},
		{ResourceKindDBSubnetGroup, "prod-db-subnets", DBSubnetGroupARN(testRegion, testAccountID, "prod-db-subnets")},
		{ResourceKindDBParameterGroup, "postgres16-tuned", DBParameterGroupARN(testRegion, testAccountID, "postgres16-tuned")},
	}

	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
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
		{"extra segment", DBInstanceARN(testRegion, testAccountID, "orders-db") + ":extra"},
		{"empty identifier", "arn:aws:rds:" + testRegion + ":" + testAccountID + ":db:"},
		{"another service", "arn:aws:ec2:" + testRegion + ":" + testAccountID + ":db:orders-db"},
		{"another partition", "arn:aws-cn:rds:" + testRegion + ":" + testAccountID + ":db:orders-db"},
		{"unknown resource type", "arn:aws:rds:" + testRegion + ":" + testAccountID + ":cluster:orders"},
		// Rejected at the ARN rather than at policy evaluation, so a foreign
		// reference never reaches the evaluator at all.
		{"foreign account", DBInstanceARN(testRegion, "210987654321", "orders-db")},
		{"foreign region", DBInstanceARN("us-east-1", testAccountID, "orders-db")},
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
	for _, kind := range []ResourceKind{ResourceKindDBInstance, ResourceKindDBSnapshot,
		ResourceKindDBSubnetGroup, ResourceKindDBParameterGroup} {
		_, ok := taggableResources[kind]
		assert.True(t, ok, "%s is a resource that exists and so must be taggable", kind)
	}
}
