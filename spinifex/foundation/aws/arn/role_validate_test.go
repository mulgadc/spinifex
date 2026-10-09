package arn_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/arn"
	"github.com/stretchr/testify/assert"
)

func TestValidateRoleARN_AcceptsValidRoleARNs(t *testing.T) {
	tests := []struct {
		name string
		arn  string
	}{
		{"no path", "arn:aws:iam::123456789012:role/CoreDNSRole"},
		{"nested path", "arn:aws:iam::123456789012:role/service-role/CoreDNSRole"},
		{"64-character name", "arn:aws:iam::123456789012:role/" + stringOfLen(64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, arn.ValidateRoleARN(tt.arn))
		})
	}
}

func TestValidateRoleARN_RejectsMalformedAndForeignARNs(t *testing.T) {
	tests := []struct {
		name string
		arn  string
	}{
		{"empty string", ""},
		{"not an ARN", "not-an-arn"},
		{"wrong service s3", "arn:aws:s3:::bucket"},
		{"wrong resource type user", "arn:aws:iam::123456789012:user/x"},
		{"sts assumed-role", "arn:aws:sts::123456789012:assumed-role/role-name/session-name"},
		{"missing account", "arn:aws:iam:::role/x"},
		{"non-12-digit account", "arn:aws:iam::12345:role/x"},
		{"account too long", "arn:aws:iam::1234567890123:role/x"},
		{"region present", "arn:aws:iam:us-east-1:123456789012:role/x"},
		{"empty name", "arn:aws:iam::123456789012:role/"},
		{"trailing slash empty name", "arn:aws:iam::123456789012:role/path/"},
		{"newline injection", "arn:aws:iam::123456789012:role/x\nkind: Secret"},
		{"quote injection", "arn:aws:iam::123456789012:role/x\""},
		{"space injection", "arn:aws:iam::123456789012:role/x y"},
		{"colon injection", "arn:aws:iam::123456789012:role/x:y"},
		{"65-character name", "arn:aws:iam::123456789012:role/" + stringOfLen(65)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Error(t, arn.ValidateRoleARN(tt.arn))
		})
	}
}

func stringOfLen(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
