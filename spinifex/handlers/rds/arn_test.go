package handlers_rds

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	testRegion    = "ap-southeast-2"
	testAccountID = "123456789012"
)

func TestARNBuilders(t *testing.T) {
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:db:orders-db",
		DBInstanceARN(testRegion, testAccountID, "orders-db"))
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:snapshot:orders-db-2026-07-24",
		DBSnapshotARN(testRegion, testAccountID, "orders-db-2026-07-24"))
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:subgrp:prod-db-subnets",
		DBSubnetGroupARN(testRegion, testAccountID, "prod-db-subnets"))
	assert.Equal(t, "arn:aws:rds:ap-southeast-2:123456789012:pg:postgres16-tuned",
		DBParameterGroupARN(testRegion, testAccountID, "postgres16-tuned"))
}
