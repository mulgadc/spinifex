package handlers_acm

import (
	"strings"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
)

// domainNamePattern is AWS's RequestCertificate DomainName constraint, quoted
// verbatim in the ValidationException message. Go's regexp lacks the (?!-)
// lookaheads, so validDomainName implements it instead.
const domainNamePattern = `(\*\.)?(((?!-)[A-Za-z0-9-]{0,62}[A-Za-z0-9])\.)+((?!-)[A-Za-z0-9-]{1,62}[A-Za-z0-9])`

// validateDomainName returns AWS's ValidationException when domain does not
// fully match domainNamePattern.
func validateDomainName(domain string) error {
	if validDomainName(domain) {
		return nil
	}
	return awserrors.Errorf(awserrors.ErrorValidationException,
		"1 validation error detected: Value of the input at 'domainName' failed to satisfy constraint: Member must satisfy regular expression pattern: %s",
		domainNamePattern)
}

// validDomainName reports whether domain fully matches domainNamePattern: an
// optional "*." then two or more dot-separated labels of letters, digits and
// hyphens, none leading or trailing with a hyphen, the last 2-63 long, the rest 1-63.
func validDomainName(domain string) bool {
	labels := strings.Split(strings.TrimPrefix(domain, "*."), ".")
	if len(labels) < 2 {
		return false
	}
	last := len(labels) - 1
	for i, label := range labels {
		minLen := 1
		if i == last {
			minLen = 2
		}
		if !validLabel(label, minLen) {
			return false
		}
	}
	return true
}

func validLabel(label string, minLen int) bool {
	if len(label) < minLen || len(label) > 63 {
		return false
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}
