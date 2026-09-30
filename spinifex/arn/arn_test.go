package arn_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/stretchr/testify/assert"
)

func TestSplit(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		service  string
		region   string
		account  string
		resource string
		wantOK   bool
	}{
		{
			name:     "regional ARN",
			value:    "arn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
			service:  "ecs",
			region:   "ap-southeast-2",
			account:  "123456789012",
			resource: "cluster/web",
			wantOK:   true,
		},
		{
			name:     "global ARN has an empty region",
			value:    "arn:aws:iam::123456789012:root",
			service:  "iam",
			account:  "123456789012",
			resource: "root",
			wantOK:   true,
		},
		{
			name:     "resource keeps its own colons",
			value:    "arn:aws:rds:ap-southeast-2:123456789012:snapshot:rds:orders-2026-09-30",
			service:  "rds",
			region:   "ap-southeast-2",
			account:  "123456789012",
			resource: "snapshot:rds:orders-2026-09-30",
			wantOK:   true,
		},
		{
			name:    "empty resource",
			value:   "arn:aws:ecs:ap-southeast-2:123456789012:",
			service: "ecs",
			region:  "ap-southeast-2",
			account: "123456789012",
			wantOK:  true,
		},
		{
			name:  "another partition",
			value: "arn:aws-cn:ecs:cn-north-1:123456789012:cluster/web",
		},
		{
			name:  "not an ARN",
			value: "urn:aws:ecs:ap-southeast-2:123456789012:cluster/web",
		},
		{
			name:  "too few components",
			value: "arn:aws:ecs:ap-southeast-2:cluster/web",
		},
		{
			name: "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service, region, account, resource, ok := arn.Split(tt.value)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.service, service)
			assert.Equal(t, tt.region, region)
			assert.Equal(t, tt.account, account)
			assert.Equal(t, tt.resource, resource)
		})
	}
}
