package awsmodel

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateEC2ErrorResponseAllowsCuratedCodes(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "common client", status: 403, code: "UnauthorizedOperation"},
		{name: "specific client", status: 404, code: "InvalidInstanceID.NotFound"},
		{name: "server", status: 500, code: "InternalError"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations, err := ValidateEC2ErrorResponse(test.status, ec2ErrorBody(test.code))
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != 0 {
				t.Fatalf("violations = %#v, want none", violations)
			}
		})
	}
}

func TestValidateEC2ErrorResponseRejectsUnverifiedCode(t *testing.T) {
	violations, err := ValidateEC2ErrorResponse(403, ec2ErrorBody("ExpiredToken"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{{
		Rule:    RuleErrorCode,
		Path:    "$error.Code",
		Message: `error code "ExpiredToken" is not in the curated EC2 catalog`,
	}}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateEC2ErrorResponseRejectsWrongStatusClass(t *testing.T) {
	violations, err := ValidateEC2ErrorResponse(400, ec2ErrorBody("InsufficientInstanceCapacity"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{{
		Rule:    RuleHTTPStatus,
		Path:    "$response.status",
		Message: `status 400 is not in the 5xx class required for EC2 server error "InsufficientInstanceCapacity"`,
	}}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateEC2ErrorResponseRejectsMalformedEnvelope(t *testing.T) {
	_, err := ValidateEC2ErrorResponse(400, []byte(`<ErrorResponse><Error><Code>InvalidAction</Code></Error></ErrorResponse>`))
	if err == nil || !strings.Contains(err.Error(), `root element is "ErrorResponse", want "Response"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestEC2ErrorCatalogMetadataAndClasses(t *testing.T) {
	catalog, err := loadEC2ErrorCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if catalog.metadata.Source != "https://docs.aws.amazon.com/ec2/latest/devguide/errors-overview.html" {
		t.Fatalf("source = %q", catalog.metadata.Source)
	}
	if catalog.metadata.VerifiedOn == "" {
		t.Fatal("verifiedOn is empty")
	}
	if len(catalog.codes) != 112 {
		t.Fatalf("catalog code count = %d, want 112", len(catalog.codes))
	}
	if catalog.codes["InvalidParameterValue"] != ec2ErrorClassClient {
		t.Fatal("InvalidParameterValue is not classified as client")
	}
	if catalog.codes["ServerInternal"] != ec2ErrorClassServer {
		t.Fatal("ServerInternal is not classified as server")
	}
}

func ec2ErrorBody(code string) []byte {
	return []byte(`<Response><Errors><Error><Code>` + code + `</Code><Message>test</Message></Error></Errors><RequestID>request-1</RequestID></Response>`)
}
