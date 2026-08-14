package cmd_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/mulgadc/spinifex/cmd/spinifex/cmd"
	"github.com/mulgadc/spinifex/spinifex/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const auditAccountID = "000000000001"

// STS gates AssumeRole on the trust policy alone, so an account-wide trust
// policy is assumable by every principal in the account — including the
// single-action signup credential.
func TestTrustsWholeAccount(t *testing.T) {
	tests := []struct {
		name     string
		document string
		want     bool
	}{
		{
			name:     "root ARN of the account",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000001:root"},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "bare account ID",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"000000000001"},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "wildcard",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "account-wide among several entries",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::000000000001:user/alice","000000000001"]},"Action":"sts:AssumeRole"}]}`,
			want:     true,
		},
		{
			name:     "specific user",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000001:user/alice"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "another account",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::123456789012:root"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "service principal",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "deny statement is not a grant",
			document: `{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"AWS":"*"},"Action":"sts:AssumeRole"}]}`,
			want:     false,
		},
		{
			name:     "unparseable document is reported rather than passed over",
			document: `not json`,
			want:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, cmd.TrustsWholeAccount(tc.document, auditAccountID))
		})
	}
}

func TestNewClientTokenIsAccepted(t *testing.T) {
	valid := regexp.MustCompile(`^[A-Za-z0-9_-]{32,128}$`)

	first := cmd.NewClientToken()
	second := cmd.NewClientToken()

	assert.Regexp(t, valid, first)
	assert.NotEqual(t, first, second)
}

func TestLocalGatewayEndpoint(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{"explicit host", "10.0.0.1:9999", "https://10.0.0.1:9999"},
		{"wildcard bind is not dialable", "0.0.0.0:9999", "https://localhost:9999"},
		{"empty host", ":9999", "https://localhost:9999"},
		{"no port", "10.0.0.1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			node := config.Config{AWSGW: config.AWSGWConfig{Host: tc.host}}
			assert.Equal(t, tc.want, cmd.LocalGatewayEndpoint(node))
		})
	}
}

// An unreadable or empty CA bundle must be an error, never a silent fallback
// to an unverified connection.
func TestAdminHTTPClientRejectsBadCABundle(t *testing.T) {
	_, err := cmd.AdminHTTPClient(filepath.Join(t.TempDir(), "absent.pem"))
	assert.Error(t, err)

	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o600))
	_, err = cmd.AdminHTTPClient(empty)
	assert.Error(t, err)
}
