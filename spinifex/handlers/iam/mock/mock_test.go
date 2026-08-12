package mock_test

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/handlers/iam/mock"
)

func TestGetRole_MissingReturnsNoSuchEntity(t *testing.T) {
	e := mock.New()
	_, err := e.GetRole("acct", &iam.GetRoleInput{RoleName: aws.String("r1")})
	if err == nil || err.Error() != awserrors.ErrorIAMNoSuchEntity {
		t.Fatalf("err = %v, want ErrorIAMNoSuchEntity", err)
	}
}

func TestCreateRole_DuplicateReturnsEntityAlreadyExists(t *testing.T) {
	e := mock.New()
	in := &iam.CreateRoleInput{RoleName: aws.String("r1"), AssumeRolePolicyDocument: aws.String("doc")}
	if _, err := e.CreateRole("acct", in); err != nil {
		t.Fatalf("first CreateRole: %v", err)
	}
	if _, err := e.CreateRole("acct", in); err == nil || err.Error() != awserrors.ErrorIAMEntityAlreadyExists {
		t.Fatalf("second CreateRole err = %v, want EntityAlreadyExists", err)
	}
	if e.CreateRoleCalls != 2 {
		t.Fatalf("CreateRoleCalls = %d, want 2", e.CreateRoleCalls)
	}
	if e.LastTrustDoc != "doc" {
		t.Fatalf("LastTrustDoc = %q, want %q", e.LastTrustDoc, "doc")
	}
}

// AddRoleToInstanceProfile must report LimitExceeded on a second attach,
// mirroring the live API rejecting a second role on one profile.
func TestAddRoleToInstanceProfile_SecondAttachIsLimitExceeded(t *testing.T) {
	e := mock.New()
	if _, err := e.CreateInstanceProfile("acct", &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("p1"),
	}); err != nil {
		t.Fatalf("CreateInstanceProfile: %v", err)
	}
	in := &iam.AddRoleToInstanceProfileInput{InstanceProfileName: aws.String("p1"), RoleName: aws.String("r1")}
	if _, err := e.AddRoleToInstanceProfile("acct", in); err != nil {
		t.Fatalf("first AddRoleToInstanceProfile: %v", err)
	}
	if _, err := e.AddRoleToInstanceProfile("acct", in); err == nil || err.Error() != awserrors.ErrorIAMLimitExceeded {
		t.Fatalf("second AddRoleToInstanceProfile err = %v, want LimitExceeded", err)
	}
}

func TestCreateInstanceProfile_EmptyARNOverride(t *testing.T) {
	e := mock.New()
	e.EmptyInstanceProfileARN = true
	out, err := e.CreateInstanceProfile("acct", &iam.CreateInstanceProfileInput{
		InstanceProfileName: aws.String("p1"),
	})
	if err != nil {
		t.Fatalf("CreateInstanceProfile: %v", err)
	}
	if aws.StringValue(out.InstanceProfile.Arn) != "" {
		t.Fatalf("Arn = %q, want empty", aws.StringValue(out.InstanceProfile.Arn))
	}
}

func TestPutRolePolicy_InjectedErrorStillRecordsCall(t *testing.T) {
	e := mock.New()
	injected := errors.New("boom")
	e.PutRolePolicyErr = injected
	in := &iam.PutRolePolicyInput{RoleName: aws.String("r1"), PolicyName: aws.String("p"), PolicyDocument: aws.String("doc")}
	_, err := e.PutRolePolicy("acct", in)
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want injected error", err)
	}
	if len(e.PolicyCalls) != 1 {
		t.Fatalf("PolicyCalls len = %d, want 1", len(e.PolicyCalls))
	}
	if e.RolePolicies["r1"] != "doc" {
		t.Fatalf("RolePolicies[r1] = %q, want %q", e.RolePolicies["r1"], "doc")
	}
}
