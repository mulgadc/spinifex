package arn_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/arn"
	"github.com/stretchr/testify/assert"
)

func TestFormatACMCertificate(t *testing.T) {
	assert.Equal(t, "arn:aws:acm:ap-southeast-2:123456789012:certificate/aaaa-1111",
		arn.FormatACMCertificate("ap-southeast-2", "123456789012", "aaaa-1111"))
}

func TestParseACMCertificateID(t *testing.T) {
	tests := []struct {
		name    string
		certARN string
		want    string
		wantOK  bool
	}{
		{
			name:    "certificate ARN",
			certARN: "arn:aws:acm:ap-southeast-2:123456789012:certificate/aaaa-1111",
			want:    "aaaa-1111",
			wantOK:  true,
		},
		{
			name:    "another account and region still yields the id",
			certARN: "arn:aws:acm:us-east-1:999999999999:certificate/aaaa-1111",
			want:    "aaaa-1111",
			wantOK:  true,
		},
		{
			name:    "id keeps everything after the first slash",
			certARN: "arn:aws:acm:ap-southeast-2:123456789012:certificate/aaaa-1111/admin",
			want:    "aaaa-1111/admin",
			wantOK:  true,
		},
		{
			name:    "a literal star is an id like any other",
			certARN: "arn:aws:acm:ap-southeast-2:123456789012:certificate/*",
			want:    "*",
			wantOK:  true,
		},
		{
			name:    "another service",
			certARN: "arn:aws:acm-pca:ap-southeast-2:123456789012:certificate-authority/aaaa-1111",
			wantOK:  false,
		},
		{
			name:    "another resource type",
			certARN: "arn:aws:acm:ap-southeast-2:123456789012:certificate-authority/aaaa-1111",
			wantOK:  false,
		},
		{
			name:    "no resource component",
			certARN: "arn:aws:acm:ap-southeast-2:123456789012:certificate",
			wantOK:  false,
		},
		{
			name:    "empty id",
			certARN: "arn:aws:acm:ap-southeast-2:123456789012:certificate/",
			wantOK:  false,
		},
		{
			name:    "too few segments",
			certARN: "arn:aws:acm:certificate/aaaa-1111",
			wantOK:  false,
		},
		{
			name:    "a bare id is not an ARN",
			certARN: "aaaa-1111",
			wantOK:  false,
		},
		{
			name:   "empty",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := arn.ParseACMCertificateID(tt.certARN)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Re-anchoring is the composition the ACM gate relies on: an ARN naming another
// account resolves to one under the caller's own.
func TestParseACMCertificateIDReanchors(t *testing.T) {
	id, ok := arn.ParseACMCertificateID("arn:aws:acm:us-east-1:999999999999:certificate/aaaa-1111")
	assert.True(t, ok)
	assert.Equal(t, "arn:aws:acm:ap-southeast-2:123456789012:certificate/aaaa-1111",
		arn.FormatACMCertificate("ap-southeast-2", "123456789012", id))
}
