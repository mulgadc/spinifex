package awsec2query

import (
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ErrSliceTooLarge is returned when a list exceeds maxSliceLen entries.
// Callers should map this to AWS's MalformedQueryString error code.
var ErrSliceTooLarge = errors.New("list parameter exceeds maximum entries")

func QueryParamsToStruct(params map[string]string, out any) error {
	v := reflect.ValueOf(out)

	// Must be a pointer to a struct
	if v.Kind() != reflect.Pointer {
		return fmt.Errorf("out must be a pointer to a struct")
	}

	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("out must be a pointer to a struct")
	}

	return setStructFields(v, params, "")
}

func setStructFields(v reflect.Value, params map[string]string, prefix string) error {
	t := v.Type()

	for i := 0; i < v.NumField(); i++ {
		field := v.Field(i)
		fieldType := t.Field(i)

		// Skip unexported fields
		if !field.CanSet() {
			continue
		}

		fieldName := fieldType.Name

		// Check for locationName tag (AWS SDK uses this for query parameter names)
		locationName := fieldType.Tag.Get("locationName")

		// Try field name, locationName, and title-cased locationName.
		// AWS query params use title case but some SDK structs use camelCase locationName.
		queryKeys := []string{prefix + fieldName}
		if locationName != "" && locationName != fieldName {
			queryKeys = append(queryKeys, prefix+locationName)
			titled := strings.ToUpper(locationName[:1]) + locationName[1:]
			if titled != fieldName && titled != locationName {
				queryKeys = append(queryKeys, prefix+titled)
			}
		}

		// Check if this is a simple field (string, int, bool, etc.)
		for _, queryKey := range queryKeys {
			if val, ok := params[queryKey]; ok {
				if err := setFieldValue(field, val); err != nil {
					return fmt.Errorf("error setting field %s: %w", fieldName, err)
				}
				goto nextField
			}
		}

		// Handle pointer to struct (nested structs)
		if field.Kind() == reflect.Pointer && field.Type().Elem().Kind() == reflect.Struct {
			for _, queryKey := range queryKeys {
				// Check if there are any params with this prefix
				hasParams := false
				searchPrefix := queryKey + "."
				for k := range params {
					if strings.HasPrefix(k, searchPrefix) {
						hasParams = true
						break
					}
				}
				if hasParams {
					structPtr := reflect.New(field.Type().Elem())
					if err := setStructFields(structPtr.Elem(), params, searchPrefix); err != nil {
						return fmt.Errorf("error setting nested struct field %s: %w", fieldName, err)
					}
					field.Set(structPtr)
					goto nextField
				}
			}
		}

		// Handle slice fields (e.g., SecurityGroup.1, SecurityGroup.2)
		if field.Kind() == reflect.Slice {
			for _, queryKey := range queryKeys {
				if err := setSliceField(field, params, queryKey); err != nil {
					return fmt.Errorf("error setting slice field %s: %w", fieldName, err)
				}
				if field.Len() > 0 {
					goto nextField
				}
			}
			continue
		}

		// Handle pointer to slice
		if field.Kind() == reflect.Pointer && field.Type().Elem().Kind() == reflect.Slice {
			for _, queryKey := range queryKeys {
				sliceField := reflect.New(field.Type().Elem()).Elem()
				if err := setSliceField(sliceField, params, queryKey); err != nil {
					return fmt.Errorf("error setting pointer to slice field %s: %w", fieldName, err)
				}
				if sliceField.Len() > 0 {
					field.Set(sliceField.Addr())
					goto nextField
				}
			}
			continue
		}

	nextField:
	}

	return nil
}

func setFieldValue(field reflect.Value, value string) error {
	switch field.Kind() {
	case reflect.Slice:
		elem := field.Type().Elem()
		// Special case: []byte
		if elem.Kind() == reflect.Uint8 {
			// Try base64 first (AWS API expectation); fall back to raw bytes.
			trimmed := strings.TrimSpace(value)
			if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
				field.SetBytes(decoded)
				return nil
			}
			field.SetBytes([]byte(value))
			return nil
		}

	case reflect.String:
		field.SetString(value)
	case reflect.Pointer:
		// Handle pointer types
		if field.IsNil() {
			field.Set(reflect.New(field.Type().Elem()))
		}
		return setFieldValue(field.Elem(), value)
	case reflect.Int, reflect.Int64:
		i, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		field.SetInt(i)
	case reflect.Bool:
		b, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		field.SetBool(b)
	case reflect.Struct:
		if field.Type() == reflect.TypeOf(time.Time{}) {
			timestamp, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return fmt.Errorf("parse RFC3339 timestamp: %w", err)
			}
			field.Set(reflect.ValueOf(timestamp))
			return nil
		}
		return fmt.Errorf("unsupported struct type: %v", field.Type())
	default:
		return fmt.Errorf("unsupported field type: %v", field.Kind())
	}
	return nil
}

// maxSliceLen caps list materialisation to prevent unbounded allocation
// from adversarial indexes like `Filter.999999`.
const maxSliceLen = 1024

func setSliceField(field reflect.Value, params map[string]string, prefix string) error {
	// Collect indexed items; supports EC2-style (Prefix.N) and IAM/ELBv2-style (Prefix.member.N).
	indices := make(map[int]bool)
	useMember := false
	for key := range params {
		if strings.HasPrefix(key, prefix+".member.") {
			parts := strings.Split(key[len(prefix)+len(".member."):], ".")
			if len(parts) > 0 {
				if idx, err := strconv.Atoi(parts[0]); err == nil {
					indices[idx] = true
					useMember = true
				}
			}
		} else if strings.HasPrefix(key, prefix+".") {
			parts := strings.Split(key[len(prefix)+1:], ".")
			if len(parts) > 0 {
				if idx, err := strconv.Atoi(parts[0]); err == nil {
					indices[idx] = true
				}
			}
		}
	}

	if len(indices) == 0 {
		return nil
	}

	// AWS stops at the first gap: Filter.1, Filter.3 yields one entry, not three.
	denseLen := 0
	for indices[denseLen+1] {
		denseLen++
		if denseLen > maxSliceLen {
			return fmt.Errorf("%w: %q (max %d)", ErrSliceTooLarge, prefix, maxSliceLen)
		}
	}
	if denseLen == 0 {
		return nil
	}

	elemType := field.Type().Elem()
	slice := reflect.MakeSlice(field.Type(), denseLen, denseLen)

	// Process each index
	for idx := 1; idx <= denseLen; idx++ {
		elem := slice.Index(idx - 1)
		var indexPrefix string
		if useMember {
			indexPrefix = fmt.Sprintf("%s.member.%d", prefix, idx)
		} else {
			indexPrefix = fmt.Sprintf("%s.%d", prefix, idx)
		}

		// Handle different element types
		switch elemType.Kind() {
		case reflect.String:
			if val, ok := params[indexPrefix]; ok {
				elem.SetString(val)
			}
		case reflect.Pointer:
			// Handle pointer to string
			if elemType.Elem().Kind() == reflect.String {
				if val, ok := params[indexPrefix]; ok {
					str := val
					elem.Set(reflect.ValueOf(&str))
				}
			} else if elemType.Elem().Kind() == reflect.Struct {
				// Handle pointer to struct
				structPtr := reflect.New(elemType.Elem())
				if err := setStructFields(structPtr.Elem(), params, indexPrefix+"."); err != nil {
					return err
				}
				elem.Set(structPtr)
			}
		case reflect.Struct:
			if err := setStructFields(elem, params, indexPrefix+"."); err != nil {
				return err
			}
		default:
			if val, ok := params[indexPrefix]; ok {
				if err := setFieldValue(elem, val); err != nil {
					return err
				}
			}
		}
	}

	field.Set(slice)
	return nil
}
