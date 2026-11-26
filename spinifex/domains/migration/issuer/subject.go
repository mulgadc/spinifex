// Package issuer mints short-lived client certificates for AWS IAM Roles
// Anywhere and signs the CreateSession call with them.
package issuer

import (
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/identifiers"
)

const (
	commonNamePrefix = "spx-"
	orgPrefix        = "spinifex-migration-"

	// AWS builds the session source identity from "CN=<subject CN>" and
	// accepts a CN of at most 61 characters.
	maxCommonNameLen = 61
	maxClusterIDLen  = 64
)

// ErrMigrationSubject means a certificate subject has the migration form.
var ErrMigrationSubject = errors.New("certificate subject is reserved for AWS migration")

// tokenPattern is a strict subset of the STS tag value pattern
// [\p{L}\p{Z}\p{N}_.:/=+\-@]*. AWS ignores a principal tag whose value
// does not match the full pattern, so a value must never leave it.
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Subject is the identity in a migration certificate. All three fields come
// from the platform, never from a request body.
type Subject struct {
	ClusterID    string
	AccountID    string
	ConnectionID string
}

// Validate checks that every field is safe as an STS principal tag value.
func (s Subject) Validate() error {
	if !tokenPattern.MatchString(s.ClusterID) || len(s.ClusterID) > maxClusterIDLen-len(orgPrefix) {
		return fmt.Errorf("migration subject: invalid cluster id")
	}
	if !identifiers.IsAccountID(s.AccountID) {
		return fmt.Errorf("migration subject: invalid account id")
	}
	if !tokenPattern.MatchString(s.ConnectionID) || len(commonNamePrefix)+len(s.ConnectionID) > maxCommonNameLen {
		return fmt.Errorf("migration subject: invalid connection id")
	}
	return nil
}

// Name returns the certificate subject: CN spx-<connection>, OU <account>,
// O spinifex-migration-<cluster>.
func (s Subject) Name() (pkix.Name, error) {
	if err := s.Validate(); err != nil {
		return pkix.Name{}, err
	}
	return pkix.Name{
		CommonName:         commonNamePrefix + s.ConnectionID,
		OrganizationalUnit: []string{s.AccountID},
		Organization:       []string{orgPrefix + s.ClusterID},
	}, nil
}

// IsMigrationSubject reports whether name has the migration form: an
// organisation with the migration prefix, or an spx- common name together
// with an organisational unit.
func IsMigrationSubject(name pkix.Name) bool {
	for _, o := range name.Organization {
		if strings.HasPrefix(o, orgPrefix) {
			return true
		}
	}
	return strings.HasPrefix(name.CommonName, commonNamePrefix) && len(name.OrganizationalUnit) > 0
}

// RefuseMigrationSubject is the guard that every other certificate issuer
// runs before signing.
func RefuseMigrationSubject(name pkix.Name) error {
	if IsMigrationSubject(name) {
		return ErrMigrationSubject
	}
	return nil
}
