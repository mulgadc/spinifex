package handlers_sts

import (
	"fmt"
	"strings"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// constraintViolation is one input member failing one of its model constraints.
type constraintViolation struct {
	field      string
	value      string
	constraint string
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
