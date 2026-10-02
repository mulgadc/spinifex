package utils

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"log/slog"
	"os"
	"reflect"
	"slices"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/private/protocol/xml/xmlutil"
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

func MarshalToXML(payload any) ([]byte, error) {
	var buf bytes.Buffer
	enc := xml.NewEncoder(&buf)

	if err := xmlutil.BuildXML(payload, enc); err != nil {
		slog.Error("BuildXML failed", "err", err)
		return nil, err
	}

	if err := enc.Flush(); err != nil {
		slog.Error("Flush failed", "err", err)
		return nil, err
	}

	return buf.Bytes(), nil
}

// GenerateXMLPayload wraps payload with the requested locationName tag.
func GenerateXMLPayload(locationName string, payload any) any {
	t := reflect.StructOf([]reflect.StructField{
		{
			Name: "Value",
			Type: reflect.TypeOf(payload),
			Tag:  reflect.StructTag(`locationName:"` + locationName + `"`),
		},
	})

	v := reflect.New(t).Elem()
	v.Field(0).Set(reflect.ValueOf(payload))
	return v.Interface()
}

// NormalizeXMLOutput returns a copy of output with every nil slice field
// (recursively) replaced by a non-nil empty slice, since aws-sdk-go's
// xmlutil.BuildXML omits a nil slice's container element entirely but
// renders an empty one for a non-nil empty slice. AWS renders most empty lists
// but omits some; asSet names those fields, per struct type, and they are
// left as the handler set them so a nil one stays omitted.
func NormalizeXMLOutput(output any, asSet map[reflect.Type][]string) any {
	v := reflect.ValueOf(output)
	if !v.IsValid() {
		return output
	}
	// Work on an addressable copy: callers pass struct values, and reflection
	// can only Set fields through an addressable Value.
	ptr := reflect.New(v.Type())
	ptr.Elem().Set(v)
	normalizeNilSlices(ptr.Elem(), asSet)
	return ptr.Elem().Interface()
}

// normalizeNilSlices walks v in place, turning nil slice fields into empty
// ones and recursing into structs, pointers, and existing slice elements.
func normalizeNilSlices(v reflect.Value, asSet map[reflect.Type][]string) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			normalizeNilSlices(v.Elem(), asSet)
		}
	case reflect.Struct:
		keep := asSet[v.Type()]
		for sf, field := range v.Fields() {
			switch field.Kind() {
			case reflect.Slice:
				if field.IsNil() {
					if field.CanSet() && !slices.Contains(keep, sf.Name) {
						field.Set(reflect.MakeSlice(field.Type(), 0, 0))
					}
				} else {
					for j := 0; j < field.Len(); j++ {
						normalizeNilSlices(field.Index(j), asSet)
					}
				}
			case reflect.Pointer, reflect.Struct:
				normalizeNilSlices(field, asSet)
			}
		}
	}
}

// WithRequestID returns a copy of payload's structure with a synthetic
// RequestId field prepended, since the SDK's generated output structs never
// carry one. payload must be a struct or pointer to struct; anything else is
// returned unchanged.
func WithRequestID(payload any, requestID string) any {
	pv := reflect.ValueOf(payload)
	for pv.Kind() == reflect.Pointer {
		pv = pv.Elem()
	}
	if pv.Kind() != reflect.Struct {
		return payload
	}
	pt := pv.Type()

	fields := []reflect.StructField{
		{
			Name: "RequestId",
			Type: reflect.TypeFor[string](),
			Tag:  reflect.StructTag(`locationName:"requestId" type:"string"`),
		},
	}
	for f := range pt.Fields() {
		if f.PkgPath == "" { // skip unexported marker fields (e.g. "_")
			fields = append(fields, f)
		}
	}

	composite := reflect.New(reflect.StructOf(fields)).Elem()
	composite.FieldByName("RequestId").SetString(requestID)
	for i := 0; i < pt.NumField(); i++ {
		if f := pt.Field(i); f.PkgPath == "" {
			composite.FieldByName(f.Name).Set(pv.Field(i))
		}
	}

	return composite.Interface()
}

// GenerateIAMXMLPayload wraps IAM output in the <ActionResponse><ActionResult>...</ActionResult></ActionResponse> structure.
func GenerateIAMXMLPayload(action string, payload any) any {
	resultName := action + "Result"
	resultWrapper := reflect.StructOf([]reflect.StructField{
		{
			Name: "Result",
			Type: reflect.TypeOf(payload),
			Tag:  reflect.StructTag(`locationName:"` + resultName + `"`),
		},
	})
	resultV := reflect.New(resultWrapper).Elem()
	resultV.Field(0).Set(reflect.ValueOf(payload))

	responseName := action + "Response"
	responseWrapper := reflect.StructOf([]reflect.StructField{
		{
			Name: "Response",
			Type: resultWrapper,
			Tag:  reflect.StructTag(`locationName:"` + responseName + `"`),
		},
	})
	responseV := reflect.New(responseWrapper).Elem()
	responseV.Field(0).Set(resultV)

	return responseV.Interface()
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

// ValidateKeyPairName validates that a key pair name contains only [A-Za-z0-9._-].
// Rejects empty names and returns ErrorInvalidKeyPairFormat on any invalid character.
func ValidateKeyPairName(name string) error {
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

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return false
	}
	return info.IsDir()
}
