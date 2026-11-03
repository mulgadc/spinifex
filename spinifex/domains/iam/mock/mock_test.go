package mock_test

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/domains/iam/mock"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
)

func TestGetRole_MissingReturnsNoSuchEntity(t *testing.T) {
	e := mock.New()
	_, err := e.GetRole("acct", &iam.GetRoleInput{RoleName: aws.String("r1")})
	if err == nil || awserrors.ValidErrorCodeFromError(err) != awserrors.ErrorIAMNoSuchEntity {
		t.Fatalf("err = %v, want ErrorIAMNoSuchEntity", err)
	}
}

func TestCreateRole_DuplicateReturnsEntityAlreadyExists(t *testing.T) {
	e := mock.New()
	in := &iam.CreateRoleInput{RoleName: aws.String("r1"), AssumeRolePolicyDocument: aws.String("doc")}
	if _, err := e.CreateRole("acct", in); err != nil {
		t.Fatalf("first CreateRole: %v", err)
	}
	if _, err := e.CreateRole("acct", in); err == nil || awserrors.ValidErrorCodeFromError(err) != awserrors.ErrorIAMEntityAlreadyExists {
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
	if _, err := e.AddRoleToInstanceProfile("acct", in); err == nil || awserrors.ValidErrorCodeFromError(err) != awserrors.ErrorIAMLimitExceeded {
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

func seedProfile(t *testing.T, e *mock.SystemInstanceRoleEnsurer, name string) {
	t.Helper()
	if _, err := e.CreateInstanceProfile("acct", &iam.CreateInstanceProfileInput{InstanceProfileName: aws.String(name)}); err != nil {
		t.Fatalf("CreateInstanceProfile: %v", err)
	}
}

func requireCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || awserrors.ValidErrorCodeFromError(err) != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
}

func TestInstanceProfileTags_TagReplacesSameKeyUntagDropsListReturnsCopy(t *testing.T) {
	e := mock.New()
	seedProfile(t, e, "p1")
	tag := func(k, v string) *iam.Tag { return &iam.Tag{Key: aws.String(k), Value: aws.String(v)} }
	if _, err := e.TagInstanceProfile("acct", &iam.TagInstanceProfileInput{
		InstanceProfileName: aws.String("p1"), Tags: []*iam.Tag{tag("a", "1"), tag("b", "2")},
	}); err != nil {
		t.Fatalf("TagInstanceProfile: %v", err)
	}
	if _, err := e.TagInstanceProfile("acct", &iam.TagInstanceProfileInput{
		InstanceProfileName: aws.String("p1"), Tags: []*iam.Tag{tag("a", "9")},
	}); err != nil {
		t.Fatalf("re-tag: %v", err)
	}
	if _, err := e.UntagInstanceProfile("acct", &iam.UntagInstanceProfileInput{
		InstanceProfileName: aws.String("p1"), TagKeys: aws.StringSlice([]string{"b"}),
	}); err != nil {
		t.Fatalf("UntagInstanceProfile: %v", err)
	}
	out, err := e.ListInstanceProfileTags("acct", &iam.ListInstanceProfileTagsInput{InstanceProfileName: aws.String("p1")})
	if err != nil {
		t.Fatalf("ListInstanceProfileTags: %v", err)
	}
	if len(out.Tags) != 1 || aws.StringValue(out.Tags[0].Key) != "a" || aws.StringValue(out.Tags[0].Value) != "9" {
		t.Fatalf("tags = %v, want only a=9", out.Tags)
	}
}

func TestInstanceProfileTags_MissingProfileIsNoSuchEntity(t *testing.T) {
	e := mock.New()
	name := aws.String("absent")
	_, err := e.TagInstanceProfile("acct", &iam.TagInstanceProfileInput{InstanceProfileName: name})
	requireCode(t, err, awserrors.ErrorIAMNoSuchEntity)
	_, err = e.UntagInstanceProfile("acct", &iam.UntagInstanceProfileInput{InstanceProfileName: name})
	requireCode(t, err, awserrors.ErrorIAMNoSuchEntity)
	_, err = e.ListInstanceProfileTags("acct", &iam.ListInstanceProfileTagsInput{InstanceProfileName: name})
	requireCode(t, err, awserrors.ErrorIAMNoSuchEntity)
}

func TestDeleteInstanceProfile_RefusesWhileRoleAttachedThenDeletes(t *testing.T) {
	e := mock.New()
	seedProfile(t, e, "p1")
	if _, err := e.AddRoleToInstanceProfile("acct", &iam.AddRoleToInstanceProfileInput{
		InstanceProfileName: aws.String("p1"), RoleName: aws.String("r1"),
	}); err != nil {
		t.Fatalf("AddRoleToInstanceProfile: %v", err)
	}
	del := &iam.DeleteInstanceProfileInput{InstanceProfileName: aws.String("p1")}
	_, err := e.DeleteInstanceProfile("acct", del)
	requireCode(t, err, awserrors.ErrorIAMDeleteConflict)
	if _, ok := e.Profiles["p1"]; !ok {
		t.Fatal("a refused delete must leave the profile in place")
	}

	_, err = e.RemoveRoleFromInstanceProfile("acct", &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String("p1"), RoleName: aws.String("other"),
	})
	requireCode(t, err, awserrors.ErrorIAMNoSuchEntity)
	if _, err := e.RemoveRoleFromInstanceProfile("acct", &iam.RemoveRoleFromInstanceProfileInput{
		InstanceProfileName: aws.String("p1"), RoleName: aws.String("r1"),
	}); err != nil {
		t.Fatalf("RemoveRoleFromInstanceProfile: %v", err)
	}
	if _, err := e.DeleteInstanceProfile("acct", del); err != nil {
		t.Fatalf("DeleteInstanceProfile: %v", err)
	}
	if _, ok := e.Profiles["p1"]; ok {
		t.Fatal("profile still present after delete")
	}
	_, err = e.DeleteInstanceProfile("acct", del)
	requireCode(t, err, awserrors.ErrorIAMNoSuchEntity)
}

func TestInstanceProfileMutations_MissingProfileAndInjectedErrors(t *testing.T) {
	e := mock.New()
	name := aws.String("absent")
	_, err := e.RemoveRoleFromInstanceProfile("acct", &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: name, RoleName: aws.String("r")})
	requireCode(t, err, awserrors.ErrorIAMNoSuchEntity)

	boom := errors.New("boom")
	e.RemoveRoleFromInstanceProfileErr, e.DeleteInstanceProfileErr, e.GetInstanceProfileErr = boom, boom, boom
	if _, err := e.RemoveRoleFromInstanceProfile("acct", &iam.RemoveRoleFromInstanceProfileInput{InstanceProfileName: name, RoleName: aws.String("r")}); !errors.Is(err, boom) {
		t.Fatalf("RemoveRole err = %v, want injected", err)
	}
	if _, err := e.DeleteInstanceProfile("acct", &iam.DeleteInstanceProfileInput{InstanceProfileName: name}); !errors.Is(err, boom) {
		t.Fatalf("Delete err = %v, want injected", err)
	}
	if _, err := e.GetInstanceProfile("acct", &iam.GetInstanceProfileInput{InstanceProfileName: name}); !errors.Is(err, boom) {
		t.Fatalf("Get err = %v, want injected", err)
	}
	if e.RemoveRoleCalls != 2 || e.DeleteInstanceProfileCalls != 1 {
		t.Fatalf("calls = remove %d delete %d, want 2 and 1", e.RemoveRoleCalls, e.DeleteInstanceProfileCalls)
	}
}
