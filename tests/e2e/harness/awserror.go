//go:build e2e

package harness

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws/awserr"
)

// ErrorCodeIs reports whether err is an awserr.Error with the given Code.
// Returns false for nil err or non-awserr wrappers.
func ErrorCodeIs(err error, code string) bool {
	if err == nil {
		return false
	}
	var aerr awserr.Error
	if errors.As(err, &aerr) {
		return aerr.Code() == code
	}
	return false
}

// AssertAWSError fails the test if err is nil or its awserr.Code() != code.
// Logs the full err on mismatch. Returns the matched awserr.Error so callers
// can chain assertions on Message() etc.
func AssertAWSError(t *testing.T, err error, code string) awserr.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected AWS error %q, got nil", code)
	}
	var aerr awserr.Error
	if !errors.As(err, &aerr) {
		t.Fatalf("expected awserr.Error with code %q, got %T: %v", code, err, err)
	}
	if aerr.Code() != code {
		t.Fatalf("expected AWS error code %q, got %q (full: %v)", code, aerr.Code(), err)
	}
	return aerr
}

// ExpectError invokes fn and asserts the returned err has the given AWS code.
// Convenience for one-shot negative tests — replaces the bash `expect_error`
// helper used throughout run-e2e.sh.
func ExpectError(t *testing.T, code string, fn func() error) {
	t.Helper()
	AssertAWSError(t, fn(), code)
}
