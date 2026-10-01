package handlers_sts

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// constraintViolation is one input member failing one of its model constraints.
type constraintViolation struct {
	field      string
	value      string
	constraint string
}

// stringConstraint is a model string shape: a pattern, as AWS prints it without
// anchors, and a length range counted in characters.
type stringConstraint struct {
	pattern        *regexp.Regexp
	patternText    string
	minLen, maxLen int
}

var (
	// roleSessionNameConstraint is roleSessionNameRegex without the length bound, so
	// the charset and length constraints are reported separately, as AWS does.
	roleSessionNameConstraint = stringConstraint{
		regexp.MustCompile(`^[A-Za-z0-9_+=,.@-]*$`), `[\w+=,.@-]*`, minRoleSessionNameLength, maxRoleSessionNameLength,
	}
	serialNumberConstraint = stringConstraint{regexp.MustCompile(`^[A-Za-z0-9_+=/:,.@-]*$`), `[\w+=/:,.@-]*`, 9, 256}
	tokenCodeConstraint    = stringConstraint{regexp.MustCompile(`^[0-9]*$`), `[\d]*`, 6, 6}
)

// check reports value's pattern violation, then its length violation.
func (c stringConstraint) check(field, value string) []constraintViolation {
	var violations []constraintViolation
	if !c.pattern.MatchString(value) {
		violations = append(violations, constraintViolation{field, value,
			"Member must satisfy regular expression pattern: " + c.patternText})
	}
	if n := utf8.RuneCountInString(value); n < c.minLen {
		violations = append(violations, constraintViolation{field, value,
			fmt.Sprintf("Member must have length greater than or equal to %d", c.minLen)})
	} else if n > c.maxLen {
		violations = append(violations, constraintViolation{field, value,
			fmt.Sprintf("Member must have length less than or equal to %d", c.maxLen)})
	}
	return violations
}

// durationViolations range-checks a present durationSeconds against [900, maxSeconds].
func durationViolations(d *int64, maxSeconds int64) []constraintViolation {
	if d == nil {
		return nil
	}
	value := strconv.FormatInt(*d, 10)
	if *d < minDurationSeconds {
		return []constraintViolation{{"durationSeconds", value,
			fmt.Sprintf("Member must have value greater than or equal to %d", minDurationSeconds)}}
	}
	if *d > maxSeconds {
		return []constraintViolation{{"durationSeconds", value,
			fmt.Sprintf("Member must have value less than or equal to %d", maxSeconds)}}
	}
	return nil
}

// mfaViolations checks the MFA members when present, an empty value included.
func mfaViolations(serialNumber, tokenCode *string) []constraintViolation {
	var violations []constraintViolation
	if serialNumber != nil {
		violations = append(violations, serialNumberConstraint.check("serialNumber", *serialNumber)...)
	}
	if tokenCode != nil {
		violations = append(violations, tokenCodeConstraint.check("tokenCode", *tokenCode)...)
	}
	return violations
}

// validationError renders violations as the single ValidationError AWS returns for
// failed model constraints, in the order given, or nil when there are none.
func validationError(violations []constraintViolation) error {
	if len(violations) == 0 {
		return nil
	}
	parts := make([]string, len(violations))
	for i, v := range violations {
		parts[i] = fmt.Sprintf("Value '%s' at '%s' failed to satisfy constraint: %s", v.value, v.field, v.constraint)
	}
	noun := "errors"
	if len(violations) == 1 {
		noun = "error"
	}
	return awserrors.Errorf(awserrors.ErrorValidationError,
		"%d validation %s detected: %s", len(violations), noun, strings.Join(parts, "; "))
}
