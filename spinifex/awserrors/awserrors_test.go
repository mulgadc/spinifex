package awserrors

import "testing"

// TestErrorLookup_Structure asserts invariants of the ErrorLookup map without
// duplicating every entry. Catches wholesale-deletion accidents, invalid HTTP
// codes, empty messages, and regressions of SDK-surfaced codes — the failure
// modes that actually matter — without locking in a 1:1 mirror that has to be
// updated in lockstep with every new error code.
func TestErrorLookup_Structure(t *testing.T) {
	if len(ErrorLookup) < 400 {
		t.Fatalf("ErrorLookup unexpectedly small: %d entries", len(ErrorLookup))
	}

	validHTTP := map[int]bool{400: true, 403: true, 404: true, 409: true, 412: true, 413: true, 500: true, 503: true}
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
