package awserrors

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestErrorLookup_Structure asserts ErrorLookup invariants: minimum size, valid HTTP codes, non-empty messages.
// Avoids a 1:1 mirror that would need updating with every new error code.
func TestErrorLookup_Structure(t *testing.T) {
	if len(ErrorLookup) < 400 {
		t.Fatalf("ErrorLookup unexpectedly small: %d entries", len(ErrorLookup))
	}

	validHTTP := map[int]bool{400: true, 403: true, 404: true, 409: true, 412: true, 413: true, 424: true, 429: true, 500: true, 501: true, 503: true}
	for code, msg := range ErrorLookup {
		if !validHTTP[msg.HTTPCode] {
			t.Errorf("%s has invalid HTTPCode %d", code, msg.HTTPCode)
		}
		if msg.Message == "" {
			t.Errorf("%s has empty Message", code)
		}
	}

	// Spot-check business-critical codes that the AWS SDK surfaces by name.
	critical := map[string]int{
		ErrorAuthFailure:                  403,
		ErrorInvalidInstanceIDNotFound:    404,
		ErrorInvalidAMIIDNotFound:         400,
		ErrorInvalidKeyPairNotFound:       404,
		ErrorInvalidVpcIDNotFound:         404,
		ErrorInvalidGroupNotFound:         404,
		ErrorInvalidSubnetIDNotFound:      404,
		ErrorServerInternal:               500,
		ErrorInsufficientInstanceCapacity: 400,
		ErrorUnauthorizedOperation:        403,
	}
	for code, wantHTTP := range critical {
		msg, ok := ErrorLookup[code]
		if !ok {
			t.Errorf("critical code %q missing from ErrorLookup", code)
			continue
		}
		if msg.HTTPCode != wantHTTP {
			t.Errorf("%s HTTPCode = %d, want %d", code, msg.HTTPCode, wantHTTP)
		}
	}
}

func TestValidErrorCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want string
	}{
		{name: "known error code", code: ErrorAuthFailure, want: ErrorAuthFailure},
		{name: "another known code", code: ErrorInvalidParameterValue, want: ErrorInvalidParameterValue},
		{name: "unknown code returns ServerInternal", code: "CompletelyMadeUp", want: ErrorServerInternal},
		{name: "empty string returns ServerInternal", code: "", want: ErrorServerInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidErrorCode(tt.code)
			if got != tt.want {
				t.Errorf("ValidErrorCode(%q) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestResolveErrorCode(t *testing.T) {
	known := errors.New(ErrorInsufficientAddressCapacity)
	tests := []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{name: "nil", err: nil},
		{name: "exact code", err: known, want: ErrorInsufficientAddressCapacity, wantOK: true},
		{name: "single wrapper", err: fmt.Errorf("allocate public address: %w", known), want: ErrorInsufficientAddressCapacity, wantOK: true},
		{name: "nested wrappers", err: fmt.Errorf("launch on node: %w", fmt.Errorf("prepare instance: %w", known)), want: ErrorInsufficientAddressCapacity, wantOK: true},
		{name: "joined errors", err: errors.Join(errors.New("opaque"), known), want: ErrorInsufficientAddressCapacity, wantOK: true},
		{name: "first joined code wins", err: errors.Join(errors.New(ErrorAuthFailure), known), want: ErrorAuthFailure, wantOK: true},
		{name: "unknown error", err: errors.New("opaque")},
		{name: "code substring is not exact", err: errors.New("launch failed: " + ErrorInsufficientAddressCapacity)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResolveErrorCode(tt.err)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ResolveErrorCode() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestResolveErrorDetail_ErrorfMessageWins covers the message half of the
// gateway fidelity fix: a call site that used Errorf gets its own formatted
// wording back, not just the resolved code.
func TestResolveErrorDetail_ErrorfMessageWins(t *testing.T) {
	err := Errorf(ErrorDependencyViolation, "the VPC has a dependent subnet %s", "subnet-1")
	code, message, ok := ResolveErrorDetail(err)
	if !ok || code != ErrorDependencyViolation {
		t.Fatalf("ResolveErrorDetail() code = (%q, %v), want (%q, true)", code, ok, ErrorDependencyViolation)
	}
	want := "the VPC has a dependent subnet subnet-1"
	if message != want {
		t.Errorf("ResolveErrorDetail() message = %q, want %q", message, want)
	}
}

// TestResolveErrorDetail_PlainWrapCarriesNoMessage is the compatibility case:
// a generic %w wrapper not produced by Errorf must not be mistaken for a
// client-facing message, even though it sits right next to the bare code.
func TestResolveErrorDetail_PlainWrapCarriesNoMessage(t *testing.T) {
	err := fmt.Errorf("launch on node-1: %w", errors.New(ErrorInsufficientAddressCapacity))
	code, message, ok := ResolveErrorDetail(err)
	if !ok || code != ErrorInsufficientAddressCapacity {
		t.Fatalf("ResolveErrorDetail() code = (%q, %v), want (%q, true)", code, ok, ErrorInsufficientAddressCapacity)
	}
	if message != "" {
		t.Errorf("ResolveErrorDetail() message = %q, want empty (not Errorf-produced)", message)
	}
}

// TestResolveErrorDetail_BareCodeCarriesNoMessage is the compatibility case for
// the overwhelming majority of call sites: a bare errors.New(code) resolves the
// code with no message, exactly as ResolveErrorCode always has.
func TestResolveErrorDetail_BareCodeCarriesNoMessage(t *testing.T) {
	code, message, ok := ResolveErrorDetail(errors.New(ErrorInvalidParameterValue))
	if !ok || code != ErrorInvalidParameterValue || message != "" {
		t.Errorf("ResolveErrorDetail() = (%q, %q, %v), want (%q, \"\", true)", code, message, ok, ErrorInvalidParameterValue)
	}
}

// TestLookupErrorMessage covers per-service scoping: ACM must not inherit
// EKS's wording for the shared ResourceInUseException code, and an unscoped
// pair must fall back to ErrorLookup's existing global default unchanged.
func TestLookupErrorMessage(t *testing.T) {
	acm := LookupErrorMessage("acm", ErrorACMResourceInUse)
	eks := LookupErrorMessage("eks", ErrorEKSResourceInUse)
	global := ErrorLookup[ErrorEKSResourceInUse]

	if acm.Message == global.Message {
		t.Errorf("LookupErrorMessage(acm, ResourceInUseException) = %q, want distinct ACM wording", acm.Message)
	}
	if eks.Message != global.Message || eks.HTTPCode != global.HTTPCode {
		t.Errorf("LookupErrorMessage(eks, ResourceInUseException) = %+v, want unchanged global %+v", eks, global)
	}

	// A service/code pair with no override falls back to the global default.
	unscoped := LookupErrorMessage("ec2", ErrorInvalidParameterValue)
	if unscoped != ErrorLookup[ErrorInvalidParameterValue] {
		t.Errorf("LookupErrorMessage(ec2, InvalidParameterValue) = %+v, want unchanged global %+v", unscoped, ErrorLookup[ErrorInvalidParameterValue])
	}
}

// TestLookupErrorMessage_BedrockResourceNotFound guards the second shared-code
// collision: ResourceNotFoundException is an alias of the EKS code, so both
// Bedrock service keys must override it rather than inherit cluster wording.
func TestLookupErrorMessage_BedrockResourceNotFound(t *testing.T) {
	global := ErrorLookup[ErrorResourceNotFoundException]

	for _, service := range []string{"bedrock", "bedrock-runtime"} {
		got := LookupErrorMessage(service, ErrorResourceNotFoundException)
		if got.Message == global.Message {
			t.Errorf("LookupErrorMessage(%s, ResourceNotFoundException) = %q, want distinct Bedrock wording", service, got.Message)
		}
		if got.HTTPCode != 404 {
			t.Errorf("LookupErrorMessage(%s, ResourceNotFoundException).HTTPCode = %d, want 404", service, got.HTTPCode)
		}
		for _, banned := range []string{"cluster", "EKS", "ListClusters"} {
			if strings.Contains(got.Message, banned) {
				t.Errorf("LookupErrorMessage(%s, ResourceNotFoundException) = %q, contains EKS wording %q", service, got.Message, banned)
			}
		}
	}

	// EKS itself must still get its own wording from the unscoped global entry.
	if eks := LookupErrorMessage("eks", ErrorEKSResourceNotFound); eks != global {
		t.Errorf("LookupErrorMessage(eks, ResourceNotFoundException) = %+v, want unchanged global %+v", eks, global)
	}
}

func TestValidErrorCodeFromError(t *testing.T) {
	wrapped := fmt.Errorf("launch: %w", errors.New(ErrorInsufficientInstanceCapacity))
	if got := ValidErrorCodeFromError(wrapped); got != ErrorInsufficientInstanceCapacity {
		t.Errorf("ValidErrorCodeFromError() = %q, want %q", got, ErrorInsufficientInstanceCapacity)
	}
	if got := ValidErrorCodeFromError(errors.New("opaque")); got != ErrorServerInternal {
		t.Errorf("ValidErrorCodeFromError() = %q, want %q", got, ErrorServerInternal)
	}
	if got := ValidErrorCodeFromError(nil); got != ErrorServerInternal {
		t.Errorf("ValidErrorCodeFromError(nil) = %q, want %q", got, ErrorServerInternal)
	}
}

func TestHasErrorCode(t *testing.T) {
	tests := []struct {
		name string
		code string
		want bool
	}{
		{"known code", ErrorInsufficientInstanceCapacity, true},
		{"another known code", ErrorAuthFailure, true},
		{"wrapped code is not exact", "launch workers: " + ErrorInsufficientInstanceCapacity, false},
		{"unknown code", "CompletelyMadeUp", false},
		{"empty string", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasErrorCode(tt.code); got != tt.want {
				t.Errorf("HasErrorCode(%q) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

// TestIsNotFound covers the destroy-orchestration tolerance helper: it matches
// any AWS canonical InvalidX.NotFound (so teardown converges on already-gone
// resources) and only those — a nil error or a non-NotFound failure is not
// swallowed.
func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"vpc not found", errors.New(ErrorInvalidVpcIDNotFound), true},
		{"subnet not found", errors.New(ErrorInvalidSubnetIDNotFound), true},
		{"igw not found", errors.New(ErrorInvalidInternetGatewayIDNotFound), true},
		{"natgw not found", errors.New(ErrorInvalidNatGatewayIDNotFound), true},
		{"route table not found", errors.New(ErrorInvalidRouteTableIDNotFound), true},
		{"eni not found", errors.New(ErrorInvalidNetworkInterfaceIDNotFound), true},
		{"sg not found", errors.New(ErrorInvalidGroupNotFound), true},
		{"volume not found", errors.New(ErrorInvalidVolumeNotFound), true},
		{"wrapped not found", fmt.Errorf("delete cp vpc: %w", errors.New(ErrorInvalidVpcIDNotFound)), true},
		{"dependency violation is not NotFound", errors.New(ErrorDependencyViolation), false},
		{"in use is not NotFound", errors.New(ErrorInvalidNetworkInterfaceInUse), false},
		{"server internal is not NotFound", errors.New(ErrorServerInternal), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsNotFound(tt.err); got != tt.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
