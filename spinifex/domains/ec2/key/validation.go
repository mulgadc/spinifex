package key

import (
	"errors"

	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// validateKeyPairName applies the EC2 key-pair-name character rules.
func validateKeyPairName(name string) error {
	if name == "" {
		return errors.New("key name cannot be empty")
	}

	for _, char := range name {
		valid := (char >= 'A' && char <= 'Z') ||
			(char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') ||
			char == '-' ||
			char == '_' ||
			char == '.'

		if !valid {
			return errors.New(awserrors.ErrorInvalidKeyPairFormat)
		}
	}

	return nil
}
