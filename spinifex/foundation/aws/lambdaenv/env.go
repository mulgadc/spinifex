// Package lambdaenv validates function environment variables.
package lambdaenv

import (
	"errors"
	"regexp"
	"strings"
)

var (
	ErrName     = errors.New("invalid environment variable name")
	ErrReserved = errors.New("reserved environment variable name")
	ErrValue    = errors.New("environment variable contains a NUL byte")
	ErrSize     = errors.New("environment variables exceed 4096 bytes")
)

var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]+$`)

var reserved = map[string]struct{}{
	"AWS_ACCESS_KEY": {}, "AWS_ACCESS_KEY_ID": {}, "AWS_SECRET_ACCESS_KEY": {}, "AWS_SESSION_TOKEN": {},
	"AWS_DEFAULT_REGION": {}, "AWS_REGION": {}, "AWS_EXECUTION_ENV": {},
	"AWS_LAMBDA_RUNTIME_API": {}, "AWS_LAMBDA_FUNCTION_NAME": {}, "AWS_LAMBDA_FUNCTION_MEMORY_SIZE": {},
	"AWS_LAMBDA_FUNCTION_VERSION": {}, "AWS_LAMBDA_INITIALIZATION_TYPE": {},
	"AWS_LAMBDA_LOG_GROUP_NAME": {}, "AWS_LAMBDA_LOG_STREAM_NAME": {},
	"AWS_LAMBDA_MAX_CONCURRENCY": {}, "AWS_LAMBDA_METADATA_API": {}, "AWS_LAMBDA_METADATA_TOKEN": {},
	"LAMBDA_TASK_ROOT": {}, "LAMBDA_RUNTIME_DIR": {},
}

// Validate checks values without including customer names or values in errors.
func Validate(vars map[string]string) error {
	for k := range vars {
		if !namePattern.MatchString(k) {
			return ErrName
		}
	}
	size := 0
	for k, v := range vars {
		if _, ok := reserved[k]; ok {
			return ErrReserved
		}
		if strings.ContainsRune(v, '\x00') {
			return ErrValue
		}
		size += len(k) + len(v)
	}
	if size > 4096 {
		return ErrSize
	}
	return nil
}
