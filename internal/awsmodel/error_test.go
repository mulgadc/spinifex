package awsmodel

import (
	"reflect"
	"testing"
)

func TestValidateErrorResponseQueryAllowsDeclaredCode(t *testing.T) {
	body := []byte(`<ErrorResponse><Error><Code>NoSuchEntity</Code><Message>missing</Message></Error></ErrorResponse>`)
	violations, modelled, err := ValidateErrorResponse(IAM, "GetUser", body)
	if err != nil {
		t.Fatal(err)
	}
	if !modelled {
		t.Fatal("GetUser errors should be modelled")
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %#v, want none", violations)
	}
}

func TestValidateErrorResponseRejectsUndeclaredCode(t *testing.T) {
	body := []byte(`<ErrorResponse><Error><Code>InventedFailure</Code></Error></ErrorResponse>`)
	violations, modelled, err := ValidateErrorResponse(IAM, "GetUser", body)
	if err != nil {
		t.Fatal(err)
	}
	if !modelled {
		t.Fatal("GetUser errors should be modelled")
	}
	want := []Violation{{
		Rule:    RuleErrorCode,
		Path:    "$error.Code",
		Message: `error code "InventedFailure" is not declared; allowed [NoSuchEntity, ServiceFailure]`,
	}}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateErrorResponseJSONAllowsDeclaredShapeName(t *testing.T) {
	body := []byte(`{"__type":"com.amazonaws.ecs#ClusterNotFoundException","message":"missing"}`)
	violations, modelled, err := ValidateErrorResponse(ECS, "DescribeTasks", body)
	if err != nil {
		t.Fatal(err)
	}
	if !modelled || len(violations) != 0 {
		t.Fatalf("modelled = %t, violations = %#v, want modelled and conforming", modelled, violations)
	}
}

func TestValidateErrorResponseELBAllowsDeclaredCode(t *testing.T) {
	body := []byte(`<ErrorResponse><Error><Code>LoadBalancerNotFound</Code></Error></ErrorResponse>`)
	violations, modelled, err := ValidateErrorResponse(ElasticLoadBalancingV2, "DescribeLoadBalancers", body)
	if err != nil {
		t.Fatal(err)
	}
	if !modelled || len(violations) != 0 {
		t.Fatalf("modelled = %t, violations = %#v, want modelled and conforming", modelled, violations)
	}
}

func TestValidateErrorResponseRejectsMalformedEnvelope(t *testing.T) {
	_, _, err := ValidateErrorResponse(ECS, "DescribeTasks", []byte(`{"message":"missing code"}`))
	if err == nil {
		t.Fatal("expected missing error code to fail decoding")
	}
}

func TestValidateErrorResponseOperationWithoutDeclaredErrors(t *testing.T) {
	body := []byte(`<ErrorResponse><Error><Code>AccessDenied</Code></Error></ErrorResponse>`)
	violations, modelled, err := ValidateErrorResponse(STS, "GetCallerIdentity", body)
	if err != nil {
		t.Fatal(err)
	}
	if modelled || len(violations) != 0 {
		t.Fatalf("modelled = %t, violations = %#v, want unmodelled and no violations", modelled, violations)
	}
}

func TestValidateErrorResponseClassifiesCommonErrorsAsUnmodelled(t *testing.T) {
	tests := []struct {
		name      string
		service   Service
		operation string
		body      string
	}{
		{
			name:      "query authorization",
			service:   IAM,
			operation: "CreateUser",
			body:      `<ErrorResponse><Error><Code>AccessDenied</Code></Error></ErrorResponse>`,
		},
		{
			name:      "STS runtime validation",
			service:   STS,
			operation: "AssumeRole",
			body:      `<ErrorResponse><Error><Code>ValidationError</Code></Error></ErrorResponse>`,
		},
		{
			name:      "JSON authorization",
			service:   ECS,
			operation: "DescribeTasks",
			body:      `{"__type":"AccessDeniedException"}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations, modelled, err := ValidateErrorResponse(test.service, test.operation, []byte(test.body))
			if err != nil {
				t.Fatal(err)
			}
			if modelled || len(violations) != 0 {
				t.Fatalf("modelled = %t, violations = %#v, want unmodelled and no violations", modelled, violations)
			}
		})
	}
}
