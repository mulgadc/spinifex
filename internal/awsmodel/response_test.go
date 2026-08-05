package awsmodel

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateResponseEC2XML(t *testing.T) {
	body := []byte(`<DescribeInstancesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>req-1</requestId>
  <reservationSet><item><instancesSet><item>
    <instanceState><code>16</code><name>paused</name></instanceState>
    <inventedField>surprise</inventedField>
  </item></instancesSet></item></reservationSet>
</DescribeInstancesResponse>`)

	violations, err := ValidateResponse(EC2, "DescribeInstances", body)
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{
		{Rule: RuleUnknownField, Path: "$.Reservations[0].Instances[0].inventedField", Message: "field is not present in the model shape"},
		{Rule: RuleEnum, Path: "$.Reservations[0].Instances[0].State.Name", Message: `value "paused" is not one of [pending, running, shutting-down, terminated, stopping, stopped]`},
	}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateResponseQueryXML(t *testing.T) {
	body := []byte(`<GetUserResponse><GetUserResult><User>
  <Path>/</Path><UserId>AIDAEXAMPLE</UserId>
  <Arn>arn:aws:iam::123456789012:user/alice</Arn>
  <CreateDate>2026-08-05T00:00:00Z</CreateDate>
</User></GetUserResult></GetUserResponse>`)

	violations, err := ValidateResponse(IAM, "GetUser", body)
	if err != nil {
		t.Fatal(err)
	}
	want := []Violation{{Rule: RuleRequiredMember, Path: "$.User.UserName", Message: "required member is missing"}}
	if !reflect.DeepEqual(violations, want) {
		t.Fatalf("violations = %#v, want %#v", violations, want)
	}
}

func TestValidateResponseJSON(t *testing.T) {
	body := []byte(`{"tasks":[{"launchType":"ON_PREMISES"}]}`)
	violations, err := ValidateResponse(ECS, "DescribeTasks", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 1 || violations[0].Rule != RuleEnum || violations[0].Path != "$.tasks[0].launchType" {
		t.Fatalf("violations = %#v, want launchType enum violation", violations)
	}
}

func TestValidateResponseConformingEmptyEC2List(t *testing.T) {
	body := []byte(`<DescribeInstancesResponse><requestId>req-1</requestId><reservationSet></reservationSet></DescribeInstancesResponse>`)
	violations, err := ValidateResponse(EC2, "DescribeInstances", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %#v, want none", violations)
	}
}

func TestValidateResponseOperationWithoutOutputShape(t *testing.T) {
	body := []byte(`<AttachInternetGatewayResponse><requestId>req-1</requestId></AttachInternetGatewayResponse>`)
	violations, err := ValidateResponse(EC2, "AttachInternetGateway", body)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("violations = %#v, want none", violations)
	}
}

func TestValidateResponseRejectsWrongWrapper(t *testing.T) {
	_, err := ValidateResponse(IAM, "GetUser", []byte(`<GetUserResponse><WrongResult/></GetUserResponse>`))
	if err == nil || !strings.Contains(err.Error(), `result wrapper "GetUserResult" is missing`) {
		t.Fatalf("error = %v", err)
	}
}
