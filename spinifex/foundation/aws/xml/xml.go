// Package awsxml provides AWS XML and Query-protocol response helpers.
package awsxml

import (
	"bytes"
	encodingxml "encoding/xml"
	"log/slog"
	"reflect"
	"slices"

	"github.com/aws/aws-sdk-go/private/protocol/xml/xmlutil"
)

// Marshal serializes an AWS SDK-style XML response payload.
func Marshal(payload any) ([]byte, error) {
	var buf bytes.Buffer
	enc := encodingxml.NewEncoder(&buf)

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

// ResponsePayload wraps payload with the requested AWS Query response tag.
func ResponsePayload(locationName string, payload any) any {
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

// NormalizeOutput returns a copy of output with every nil slice field
// (recursively) replaced by a non-nil empty slice, since aws-sdk-go's
// xmlutil.BuildXML omits a nil slice's container element entirely but renders
// an empty one for a non-nil empty slice. AWS renders most empty lists but
// omits some; asSet names those fields, per struct type, and they are left as
// the handler set them so a nil one stays omitted.
func NormalizeOutput(output any, asSet map[reflect.Type][]string) any {
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

// QueryResponsePayload wraps payload in the IAM-style
// <ActionResponse><ActionResult>...</ActionResult></ActionResponse> envelope.
func QueryResponsePayload(action string, payload any) any {
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
