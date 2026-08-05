package awsmodel

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateUnknownField(t *testing.T) {
	document := validIAMGetUserDocument()
	document["Invented"] = true

	violations, err := Validate(IAM, "GetUser", document)
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{{
		Rule:    RuleUnknownField,
		Path:    "$.Invented",
		Message: "field is not present in the model shape",
	}}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateRequiredMembers(t *testing.T) {
	document := validIAMGetUserDocument()
	delete(document["User"].(map[string]any), "UserName")

	violations, err := Validate(IAM, "GetUser", document)
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{{
		Rule:    RuleRequiredMember,
		Path:    "$.User.UserName",
		Message: "required member is missing",
	}}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateEnum(t *testing.T) {
	document := map[string]any{
		"tasks": []any{
			map[string]any{"launchType": "ON_PREMISES"},
		},
	}

	violations, err := Validate(ECS, "DescribeTasks", document)
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{{
		Rule:    RuleEnum,
		Path:    "$.tasks[0].launchType",
		Message: "value \"ON_PREMISES\" is not one of [EC2, FARGATE, EXTERNAL]",
	}}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateConformingDocuments(t *testing.T) {
	tests := []struct {
		name      string
		service   Service
		operation string
		document  any
	}{
		{
			name:      "IAM required members",
			service:   IAM,
			operation: "GetUser",
			document:  validIAMGetUserDocument(),
		},
		{
			name:      "ECS enum",
			service:   ECS,
			operation: "DescribeTasks",
			document: map[string]any{
				"tasks":    []any{map[string]any{"launchType": "EC2"}},
				"failures": []any{},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations, err := Validate(test.service, test.operation, test.document)
			if err != nil {
				t.Fatal(err)
			}
			if len(violations) != 0 {
				t.Fatalf("violations = %#v, want none", violations)
			}
		})
	}
}

func TestValidateUnknownOperation(t *testing.T) {
	_, err := Validate(STS, "InventedOperation", map[string]any{})
	if err == nil || !strings.Contains(err.Error(), `operation "InventedOperation" is not modelled`) {
		t.Fatalf("Validate unknown operation error = %v", err)
	}
}

func TestViolationString(t *testing.T) {
	violation := Violation{Rule: RuleEnum, Path: "$.status", Message: `value "broken" is invalid`}
	want := `$.status: value "broken" is invalid (enum)`
	if got := violation.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func validIAMGetUserDocument() map[string]any {
	return map[string]any{
		"User": map[string]any{
			"Path":       "/",
			"UserName":   "alice",
			"UserId":     "AIDAEXAMPLE",
			"Arn":        "arn:aws:iam::123456789012:user/alice",
			"CreateDate": "2026-08-05T00:00:00Z",
		},
	}
}
