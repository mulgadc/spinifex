package handlers_rds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	testRegion    = "ap-southeast-2"
	testAccountID = "123456789012"
)

func TestFormatARN(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:db:orders-db",
		FormatARN(ResourceKindDBInstance, testRegion, testAccountID, "orders-db"))
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:snapshot:orders-db-2026-07-24",
		FormatARN(ResourceKindDBSnapshot, testRegion, testAccountID, "orders-db-2026-07-24"))
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:subgrp:prod-db-subnets",
		FormatARN(ResourceKindDBSubnetGroup, testRegion, testAccountID, "prod-db-subnets"))
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:pg:postgres16-tuned",
		FormatARN(ResourceKindDBParameterGroup, testRegion, testAccountID, "postgres16-tuned"))
}
