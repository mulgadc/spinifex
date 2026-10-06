package handlers_acm

import (
	"regexp"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// domainNamePattern is AWS's RequestCertificate DomainName constraint, quoted
// verbatim in the ValidationException message. domainNameRE matches the same
// strings with the (?!-) lookaheads rewritten, since Go regexp has none.
const domainNamePattern = `(\*\.)?(((?!-)[A-Za-z0-9-]{0,62}[A-Za-z0-9])\.)+((?!-)[A-Za-z0-9-]{1,62}[A-Za-z0-9])`

var domainNameRE = regexp.MustCompile(`^(\*\.)?([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9][A-Za-z0-9-]{0,61}[A-Za-z0-9]$`)

// validateDomainName returns AWS's ValidationException when domain does not
// fully match domainNamePattern.
func validateDomainName(domain string) error {
	if domainNameRE.MatchString(domain) {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorValidationException,
		"1 validation error detected: Value of the input at 'domainName' failed to satisfy constraint: Member must satisfy regular expression pattern: %s",
		domainNamePattern)
}
