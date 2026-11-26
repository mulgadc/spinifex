package issuer

import (
	"crypto/x509/pkix"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stsTagValue is the pattern AWS applies to principal tag values.
var stsTagValue = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

func TestSubject_Name(t *testing.T) {
	name, err := Subject{ClusterID: "c1", AccountID: "123456789012", ConnectionID: "conn-1"}.Name()
	require.NoError(t, err)
	assert.Equal(t, "spx-conn-1", name.CommonName)
	assert.Equal(t, []string{"123456789012"}, name.OrganizationalUnit)
	assert.Equal(t, []string{"spinifex-migration-c1"}, name.Organization)
	for _, v := range []string{name.CommonName, name.OrganizationalUnit[0], name.Organization[0]} {
		assert.Regexp(t, stsTagValue, v)
	}
}

func TestSubject_ValidateRejects(t *testing.T) {
	ok := Subject{ClusterID: "c1", AccountID: "123456789012", ConnectionID: "conn-1"}
	tests := []struct {
		name string
		mut  func(*Subject)
	}{
		{"empty cluster", func(s *Subject) { s.ClusterID = "" }},
		{"cluster with comma", func(s *Subject) { s.ClusterID = "a,O=evil" }},
		{"cluster with space", func(s *Subject) { s.ClusterID = "a b" }},
		{"cluster too long", func(s *Subject) { s.ClusterID = strings.Repeat("a", 46) }},
		{"short account", func(s *Subject) { s.AccountID = "1234" }},
		{"non digit account", func(s *Subject) { s.AccountID = "12345678901x" }},
		{"empty connection", func(s *Subject) { s.ConnectionID = "" }},
		{"connection with slash", func(s *Subject) { s.ConnectionID = "a/b" }},
		{"connection leading dash", func(s *Subject) { s.ConnectionID = "-a" }},
		{"connection pushes CN over 61", func(s *Subject) { s.ConnectionID = strings.Repeat("a", 58) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := ok
			tt.mut(&s)
			_, err := s.Name()
			assert.Error(t, err)
		})
	}

	s := ok
	s.ConnectionID = strings.Repeat("a", 57)
	_, err := s.Name()
	assert.NoError(t, err, "57 characters plus the spx- prefix is the 61 character limit")
}

func TestIsMigrationSubject(t *testing.T) {
	tests := []struct {
		name string
		in   pkix.Name
		want bool
	}{
		{"migration form", pkix.Name{CommonName: "spx-c", OrganizationalUnit: []string{"123456789012"}, Organization: []string{"spinifex-migration-x"}}, true},
		{"migration organisation alone", pkix.Name{CommonName: "h", Organization: []string{"spinifex-migration-x"}}, true},
		{"spx CN with unit", pkix.Name{CommonName: "spx-c", OrganizationalUnit: []string{"u"}}, true},
		{"ipsec peer", pkix.Name{CommonName: "node-1", Organization: []string{"Spinifex Platform"}}, false},
		{"host named spx without unit", pkix.Name{CommonName: "spx-node1", Organization: []string{"Spinifex Platform"}}, false},
		{"rds", pkix.Name{CommonName: "db1", Organization: []string{"Spinifex RDS"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsMigrationSubject(tt.in))
			if tt.want {
				assert.ErrorIs(t, RefuseMigrationSubject(tt.in), ErrMigrationSubject)
			} else {
				assert.NoError(t, RefuseMigrationSubject(tt.in))
			}
		})
	}
}
