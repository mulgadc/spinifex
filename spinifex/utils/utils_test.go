package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUnmarshalJsonPayload(t *testing.T) {
	type TestStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}

	tests := []struct {
		name        string
		jsonData    string
		expectError bool
		validate    func(t *testing.T, result *TestStruct)
	}{
		{
			name:        "Valid JSON",
			jsonData:    `{"name":"test","value":123}`,
			expectError: false,
			validate: func(t *testing.T, result *TestStruct) {
				assert.Equal(t, "test", result.Name)
				assert.Equal(t, 123, result.Value)
			},
		},
		{
			name:        "Invalid JSON - malformed",
			jsonData:    `{"name":"test","value":}`,
			expectError: true,
			validate:    nil,
		},
		{
			name:        "Invalid JSON - unknown field",
			jsonData:    `{"name":"test","value":123,"unknown":"field"}`,
			expectError: true, // DisallowUnknownFields should cause error
			validate:    nil,
		},
		{
			name:        "Empty JSON",
			jsonData:    `{}`,
			expectError: false,
			validate: func(t *testing.T, result *TestStruct) {
				assert.Empty(t, result.Name)
				assert.Equal(t, 0, result.Value)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var result TestStruct
			errResp := UnmarshalJsonPayload(&result, []byte(tt.jsonData))

			if tt.expectError {
				assert.NotNil(t, errResp, "Expected error response")
			} else {
				assert.Nil(t, errResp, "Expected no error response")
				if tt.validate != nil {
					tt.validate(t, &result)
				}
			}
		})
	}
}

func TestGenerateErrorPayload(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		validate func(t *testing.T, payload []byte)
	}{
		{
			name: "ValidationError",
			code: "ValidationError",
			validate: func(t *testing.T, payload []byte) {
				assert.Contains(t, string(payload), "ValidationError")
				assert.Contains(t, string(payload), "Code")
			},
		},
		{
			name: "InvalidInstanceType",
			code: "InvalidInstanceType",
			validate: func(t *testing.T, payload []byte) {
				assert.Contains(t, string(payload), "InvalidInstanceType")
			},
		},
		{
			name: "CustomError",
			code: "CustomError",
			validate: func(t *testing.T, payload []byte) {
				assert.Contains(t, string(payload), "CustomError")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := GenerateErrorPayload(tt.code)
			assert.NotNil(t, payload)
			assert.NotEmpty(t, payload)
			if tt.validate != nil {
				tt.validate(t, payload)
			}
		})
	}
}

func TestValidateErrorPayload(t *testing.T) {
	tests := []struct {
		name         string
		payload      string
		expectError  bool
		expectedCode string
	}{
		{
			name:         "Valid error payload",
			payload:      `{"Code":"ValidationError","Message":null}`,
			expectError:  true,
			expectedCode: "ValidationError",
		},
		{
			name:        "Valid success payload (no Code field)",
			payload:     `{"ReservationId":"r-123","Instances":[]}`,
			expectError: false,
		},
		{
			name:        "Empty payload",
			payload:     `{}`,
			expectError: true, // Empty payload treated as error by ValidateErrorPayload
		},
		{
			name:        "Invalid JSON",
			payload:     `{invalid}`,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responseError, err := ValidateErrorPayload([]byte(tt.payload))

			if tt.expectError {
				if tt.expectedCode != "" {
					// Check for specific error code
					assert.Error(t, err)
					if responseError.Code != nil {
						assert.Equal(t, tt.expectedCode, *responseError.Code)
					}
				}
			} else {
				// No error expected
				assert.NoError(t, err)
			}
		})
	}
}

// Test file extraction process
