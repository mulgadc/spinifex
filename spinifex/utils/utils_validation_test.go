package utils

import (
	"testing"
)

func TestValidateKeyPairName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expectErr bool
	}{
		// Valid names
		{
			name:      "ValidName-Simple",
			input:     "my-key",
			expectErr: false,
		},
		{
			name:      "ValidName-WithUnderscore",
			input:     "my_key",
			expectErr: false,
		},
		{
			name:      "ValidName-WithPeriod",
			input:     "my.key",
			expectErr: false,
		},
		{
			name:      "ValidName-Alphanumeric",
			input:     "mykey123",
			expectErr: false,
		},
		{
			name:      "ValidName-MixedCase",
			input:     "MyKey123",
			expectErr: false,
		},
		{
			name:      "ValidName-Complex",
			input:     "My_Key-2024.prod",
			expectErr: false,
		},

		// Invalid names
		{
			name:      "InvalidName-PathTraversal",
			input:     "../../../etc/passwd",
			expectErr: true,
		},
		{
			name:      "InvalidName-AbsolutePath",
			input:     "/etc/passwd",
			expectErr: true,
		},
		{
			name:      "InvalidName-SpecialChars",
			input:     "my-key@example.com",
			expectErr: true,
		},
		{
			name:      "InvalidName-Spaces",
			input:     "my key",
			expectErr: true,
		},
		{
			name:      "InvalidName-Dollar",
			input:     "key$name",
			expectErr: true,
		},
		{
			name:      "InvalidName-Hash",
			input:     "key#name",
			expectErr: true,
		},
		{
			name:      "InvalidName-Semicolon",
			input:     "key;name",
			expectErr: true,
		},
		{
			name:      "ValidName-OnlyPeriods",
			input:     "...",
			expectErr: false,
		},
		{
			name:      "InvalidName-Dot",
			input:     ".",
			expectErr: true,
		},
		{
			name:      "InvalidName-DotDot",
			input:     "..",
			expectErr: true,
		},
		{
			name:      "InvalidName-Empty",
			input:     "",
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKeyPairName(tt.input)
			if tt.expectErr && err == nil {
				t.Errorf("Expected error for input %q, but got none", tt.input)
			}
			if !tt.expectErr && err != nil {
				t.Errorf("Did not expect error for input %q, but got: %v", tt.input, err)
			}
		})
	}
}
