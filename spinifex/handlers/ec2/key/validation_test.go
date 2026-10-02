package handlers_ec2_key

import "testing"

func TestValidateKeyPairName(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		expectErr bool
	}{
		{name: "valid simple", input: "my-key"},
		{name: "valid underscore", input: "my_key"},
		{name: "valid period", input: "my.key"},
		{name: "valid alphanumeric", input: "mykey123"},
		{name: "valid mixed case", input: "MyKey123"},
		{name: "valid complex", input: "My_Key-2024.prod"},
		{name: "path traversal", input: "../../../etc/passwd", expectErr: true},
		{name: "absolute path", input: "/etc/passwd", expectErr: true},
		{name: "special characters", input: "my-key@example.com", expectErr: true},
		{name: "spaces", input: "my key", expectErr: true},
		{name: "dollar", input: "key$name", expectErr: true},
		{name: "hash", input: "key#name", expectErr: true},
		{name: "semicolon", input: "key;name", expectErr: true},
		{name: "empty", input: "", expectErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateKeyPairName(tt.input)
			if tt.expectErr && err == nil {
				t.Errorf("expected error for input %q, but got none", tt.input)
			}
			if !tt.expectErr && err != nil {
				t.Errorf("did not expect error for input %q, but got: %v", tt.input, err)
			}
		})
	}
}
