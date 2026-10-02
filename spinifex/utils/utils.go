package utils

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

// GenerateResourceID generates a unique resource ID with the given prefix.
// Format: {prefix}-{17 hex chars} using crypto/rand.
func GenerateResourceID(prefix string) string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		//nolint:forbidigo // Continuing after the CSPRNG fails could create predictable resource IDs.
		panic("crypto/rand failed: " + err.Error())
	}
	return prefix + "-" + hex.EncodeToString(b)[:17]
}

// GenerateErrorPayload serializes an ec2.ResponseError with the given code as JSON.
func GenerateErrorPayload(code string) (jsonResponse []byte) {
	return GenerateErrorPayloadWithMessage(code, "")
}

// GenerateErrorPayloadWithMessage serializes an ec2.ResponseError carrying both
// the sanitized code and the original error message, so the client can surface
// the actionable reason instead of a bare code. Message is omitted when empty
// or identical to the code.
func GenerateErrorPayloadWithMessage(code, message string) (jsonResponse []byte) {
	var responseError ec2.ResponseError
	responseError.Code = aws.String(code)
	if message != "" && message != code {
		responseError.Message = aws.String(message)
	}
	jsonResponse, err := json.Marshal(responseError)
	if err != nil {
		slog.Error("GenerateErrorPayload could not marshal JSON payload", "err", err)
		return nil
	}

	return jsonResponse
}

// ValidateErrorPayload decodes payload as an ec2.ResponseError and returns an error when a non-nil Code is detected.
func ValidateErrorPayload(payload []byte) (responseError ec2.ResponseError, err error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()

	err = decoder.Decode(&responseError)

	if err == nil && responseError.Code != nil {
		return responseError, errors.New("ResponseError detected")
	}
	return responseError, nil
}

// UnmarshalJsonPayload decodes jsonData into input (already a pointer) using strict field checking.
func UnmarshalJsonPayload(input any, jsonData []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(input)
	if err != nil {
		return GenerateErrorPayload(awserrors.ErrorValidationError)
	}

	return nil
}
