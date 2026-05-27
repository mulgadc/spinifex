package gateway_ec2_instance

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testGwAccountID      = "111122223333"
	testProfileARNApp    = "arn:aws:iam::111122223333:instance-profile/app-profile"
	testProfileARNOther  = "arn:aws:iam::111122223333:instance-profile/other-profile"
	testCrossAccountARN  = "arn:aws:iam::999988887777:instance-profile/cross-profile"
	testRoleARNApp       = "arn:aws:iam::111122223333:role/app-role"
	testProfileNameApp   = "app-profile"
	testProfileNameOther = "other-profile"
	testProfileIDApp     = "AIPAEXAMPLE0000000001"
	testProfileIDOther   = "AIPAEXAMPLE0000000002"
)

// fakeIAMService is a minimal IAMService stub for gateway-side tests. Only the
// methods the EC2 IAM-profile gateway code actually touches are wired with
// configurable behaviour — the rest return zero-value outputs to satisfy the
// interface contract.
type fakeIAMService struct {
	resolveFn func(accountID, nameOrARN string) (*handlers_iam.InstanceProfile, error)
}

func (f *fakeIAMService) ResolveInstanceProfile(accountID, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
	if f.resolveFn != nil {
		return f.resolveFn(accountID, nameOrARN)
	}
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (f *fakeIAMService) CreateUser(string, *iam.CreateUserInput) (*iam.CreateUserOutput, error) {
	return &iam.CreateUserOutput{}, nil
}
func (f *fakeIAMService) GetUser(string, *iam.GetUserInput) (*iam.GetUserOutput, error) {
	return &iam.GetUserOutput{}, nil
}
func (f *fakeIAMService) ListUsers(string, *iam.ListUsersInput) (*iam.ListUsersOutput, error) {
	return &iam.ListUsersOutput{}, nil
}
func (f *fakeIAMService) DeleteUser(string, *iam.DeleteUserInput) (*iam.DeleteUserOutput, error) {
	return &iam.DeleteUserOutput{}, nil
}
func (f *fakeIAMService) CreateAccessKey(string, *iam.CreateAccessKeyInput) (*iam.CreateAccessKeyOutput, error) {
	return &iam.CreateAccessKeyOutput{}, nil
}
func (f *fakeIAMService) ListAccessKeys(string, *iam.ListAccessKeysInput) (*iam.ListAccessKeysOutput, error) {
	return &iam.ListAccessKeysOutput{}, nil
}
func (f *fakeIAMService) DeleteAccessKey(string, *iam.DeleteAccessKeyInput) (*iam.DeleteAccessKeyOutput, error) {
	return &iam.DeleteAccessKeyOutput{}, nil
}
func (f *fakeIAMService) UpdateAccessKey(string, *iam.UpdateAccessKeyInput) (*iam.UpdateAccessKeyOutput, error) {
	return &iam.UpdateAccessKeyOutput{}, nil
}
func (f *fakeIAMService) CreatePolicy(string, *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error) {
	return &iam.CreatePolicyOutput{}, nil
}
func (f *fakeIAMService) GetPolicy(string, *iam.GetPolicyInput) (*iam.GetPolicyOutput, error) {
	return &iam.GetPolicyOutput{}, nil
}
func (f *fakeIAMService) GetPolicyVersion(string, *iam.GetPolicyVersionInput) (*iam.GetPolicyVersionOutput, error) {
	return &iam.GetPolicyVersionOutput{}, nil
}
func (f *fakeIAMService) ListPolicies(string, *iam.ListPoliciesInput) (*iam.ListPoliciesOutput, error) {
	return &iam.ListPoliciesOutput{}, nil
}
func (f *fakeIAMService) DeletePolicy(string, *iam.DeletePolicyInput) (*iam.DeletePolicyOutput, error) {
	return &iam.DeletePolicyOutput{}, nil
}
func (f *fakeIAMService) AttachUserPolicy(string, *iam.AttachUserPolicyInput) (*iam.AttachUserPolicyOutput, error) {
	return &iam.AttachUserPolicyOutput{}, nil
}
func (f *fakeIAMService) DetachUserPolicy(string, *iam.DetachUserPolicyInput) (*iam.DetachUserPolicyOutput, error) {
	return &iam.DetachUserPolicyOutput{}, nil
}
func (f *fakeIAMService) ListAttachedUserPolicies(string, *iam.ListAttachedUserPoliciesInput) (*iam.ListAttachedUserPoliciesOutput, error) {
	return &iam.ListAttachedUserPoliciesOutput{}, nil
}
func (f *fakeIAMService) CreateRole(string, *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return &iam.CreateRoleOutput{}, nil
}
func (f *fakeIAMService) GetRole(string, *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return &iam.GetRoleOutput{}, nil
}
func (f *fakeIAMService) ListRoles(string, *iam.ListRolesInput) (*iam.ListRolesOutput, error) {
	return &iam.ListRolesOutput{}, nil
}
func (f *fakeIAMService) DeleteRole(string, *iam.DeleteRoleInput) (*iam.DeleteRoleOutput, error) {
	return &iam.DeleteRoleOutput{}, nil
}
func (f *fakeIAMService) UpdateRole(string, *iam.UpdateRoleInput) (*iam.UpdateRoleOutput, error) {
	return &iam.UpdateRoleOutput{}, nil
}
func (f *fakeIAMService) UpdateAssumeRolePolicy(string, *iam.UpdateAssumeRolePolicyInput) (*iam.UpdateAssumeRolePolicyOutput, error) {
	return &iam.UpdateAssumeRolePolicyOutput{}, nil
}
func (f *fakeIAMService) AttachRolePolicy(string, *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return &iam.AttachRolePolicyOutput{}, nil
}
func (f *fakeIAMService) DetachRolePolicy(string, *iam.DetachRolePolicyInput) (*iam.DetachRolePolicyOutput, error) {
	return &iam.DetachRolePolicyOutput{}, nil
}
func (f *fakeIAMService) ListAttachedRolePolicies(string, *iam.ListAttachedRolePoliciesInput) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}
func (f *fakeIAMService) CreateInstanceProfile(string, *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	return &iam.CreateInstanceProfileOutput{}, nil
}
func (f *fakeIAMService) GetInstanceProfile(string, *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	return &iam.GetInstanceProfileOutput{}, nil
}
func (f *fakeIAMService) ListInstanceProfiles(string, *iam.ListInstanceProfilesInput) (*iam.ListInstanceProfilesOutput, error) {
	return &iam.ListInstanceProfilesOutput{}, nil
}
func (f *fakeIAMService) DeleteInstanceProfile(string, *iam.DeleteInstanceProfileInput) (*iam.DeleteInstanceProfileOutput, error) {
	return &iam.DeleteInstanceProfileOutput{}, nil
}
func (f *fakeIAMService) ListInstanceProfilesForRole(string, *iam.ListInstanceProfilesForRoleInput) (*iam.ListInstanceProfilesForRoleOutput, error) {
	return &iam.ListInstanceProfilesForRoleOutput{}, nil
}
func (f *fakeIAMService) AddRoleToInstanceProfile(string, *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}
func (f *fakeIAMService) RemoveRoleFromInstanceProfile(string, *iam.RemoveRoleFromInstanceProfileInput) (*iam.RemoveRoleFromInstanceProfileOutput, error) {
	return &iam.RemoveRoleFromInstanceProfileOutput{}, nil
}
func (f *fakeIAMService) GetUserPolicies(string, string) ([]handlers_iam.PolicyDocument, error) {
	return nil, nil
}
func (f *fakeIAMService) LookupAccessKey(string) (*handlers_iam.AccessKey, error) { return nil, nil }
func (f *fakeIAMService) DecryptSecret(string) (string, error)                    { return "", nil }
func (f *fakeIAMService) SeedBootstrap(*handlers_iam.BootstrapData) error         { return nil }
func (f *fakeIAMService) IsEmpty() (bool, error)                                  { return true, nil }
func (f *fakeIAMService) CreateAccount(string) (*handlers_iam.Account, error)     { return nil, nil }
func (f *fakeIAMService) GetAccount(string) (*handlers_iam.Account, error)        { return nil, nil }
func (f *fakeIAMService) ListAccounts() ([]*handlers_iam.Account, error)          { return nil, nil }

var _ handlers_iam.IAMService = (*fakeIAMService)(nil)

// profileWithRole builds a resolved profile fixture pointing at the test role.
func profileWithRole() *handlers_iam.InstanceProfile {
	return &handlers_iam.InstanceProfile{
		InstanceProfileName: testProfileNameApp,
		InstanceProfileID:   testProfileIDApp,
		AccountID:           testGwAccountID,
		ARN:                 testProfileARNApp,
		RoleName:            "app-role",
	}
}

// profileNoRole builds a resolved profile fixture with no role attached.
// resolveAndAuthorizeProfile must NOT invoke PassRole for this profile.
func profileNoRole() *handlers_iam.InstanceProfile {
	return &handlers_iam.InstanceProfile{
		InstanceProfileName: testProfileNameOther,
		InstanceProfileID:   testProfileIDOther,
		AccountID:           testGwAccountID,
		ARN:                 testProfileARNOther,
		RoleName:            "",
	}
}

// --- helper-level tests --------------------------------------------------

func TestResolveAndAuthorizeProfile_NameForm(t *testing.T) {
	gotNameOrARN := ""
	svc := &fakeIAMService{resolveFn: func(_, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
		gotNameOrARN = nameOrARN
		return profileWithRole(), nil
	}}
	checkCalled := false
	check := func(roleARN string) error {
		checkCalled = true
		assert.Equal(t, testRoleARNApp, roleARN, "PassRole must be checked on the role inside the resolved profile")
		return nil
	}

	profile, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
		svc, testGwAccountID, check)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, testProfileARNApp, profile.ARN)
	assert.Equal(t, testProfileNameApp, gotNameOrARN, "ResolveInstanceProfile must receive the name as-given")
	assert.True(t, checkCalled, "PassRole check must run when a role is attached")
}

func TestResolveAndAuthorizeProfile_ArnForm(t *testing.T) {
	gotNameOrARN := ""
	svc := &fakeIAMService{resolveFn: func(_, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
		gotNameOrARN = nameOrARN
		return profileWithRole(), nil
	}}
	_, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{Arn: aws.String(testProfileARNApp)},
		svc, testGwAccountID, func(string) error { return nil })
	require.NoError(t, err)
	assert.Equal(t, testProfileARNApp, gotNameOrARN, "Arn takes precedence and is forwarded verbatim")
}

func TestResolveAndAuthorizeProfile_MissingNameAndArn(t *testing.T) {
	svc := &fakeIAMService{}
	_, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{},
		svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestResolveAndAuthorizeProfile_NilIAMService(t *testing.T) {
	_, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
		nil, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestResolveAndAuthorizeProfile_NotFoundMapsToEC2Error(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}}
	_, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{Name: aws.String("missing")},
		svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIamInstanceProfileNotFound, err.Error(),
		"IAM NoSuchEntity must surface as EC2's InvalidIamInstanceProfile.NotFound")
}

func TestResolveAndAuthorizeProfile_PassthroughOtherIAMError(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}}
	_, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{Arn: aws.String(testCrossAccountARN)},
		svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error(),
		"non-NotFound IAM errors (e.g. cross-account AccessDenied) must pass through unchanged")
}

func TestResolveAndAuthorizeProfile_PassRoleDenied(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileWithRole(), nil
	}}
	check := func(string) error { return errors.New(awserrors.ErrorAccessDenied) }
	_, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
		svc, testGwAccountID, check)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestResolveAndAuthorizeProfile_NoRoleSkipsPassRole(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileNoRole(), nil
	}}
	checkCalled := false
	check := func(string) error {
		checkCalled = true
		return errors.New(awserrors.ErrorAccessDenied) // would fail if invoked
	}
	profile, err := resolveAndAuthorizeProfile(
		&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameOther)},
		svc, testGwAccountID, check)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Empty(t, profile.RoleName)
	assert.False(t, checkCalled, "profile-with-no-role must skip PassRole — there is no role to pass")
}

func TestEnrichProfileID_FillsIdFromResolvedProfile(t *testing.T) {
	assoc := &ec2.IamInstanceProfileAssociation{
		IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)},
	}
	enrichProfileID(assoc, profileWithRole())
	require.NotNil(t, assoc.IamInstanceProfile)
	assert.Equal(t, testProfileIDApp, aws.StringValue(assoc.IamInstanceProfile.Id))
	assert.Equal(t, testProfileARNApp, aws.StringValue(assoc.IamInstanceProfile.Arn))
}

func TestEnrichProfileID_NilAssociationIsNoOp(t *testing.T) {
	enrichProfileID(nil, profileWithRole()) // must not panic
}

func TestEnrichProfileID_NilInnerProfileIsAllocated(t *testing.T) {
	assoc := &ec2.IamInstanceProfileAssociation{}
	enrichProfileID(assoc, profileWithRole())
	require.NotNil(t, assoc.IamInstanceProfile)
	assert.Equal(t, testProfileIDApp, aws.StringValue(assoc.IamInstanceProfile.Id))
}

// --- AssociateIamInstanceProfile ---------------------------------------------

func TestAssociateIamInstanceProfile_NilInput(t *testing.T) {
	_, err := AssociateIamInstanceProfile(nil, nil, &fakeIAMService{}, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestAssociateIamInstanceProfile_MissingInstanceID(t *testing.T) {
	in := &ec2.AssociateIamInstanceProfileInput{
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
	}
	_, err := AssociateIamInstanceProfile(in, nil, &fakeIAMService{}, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestAssociateIamInstanceProfile_MissingProfileSpec(t *testing.T) {
	in := &ec2.AssociateIamInstanceProfileInput{InstanceId: aws.String("i-001")}
	_, err := AssociateIamInstanceProfile(in, nil, &fakeIAMService{}, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestAssociateIamInstanceProfile_ProfileNotFound(t *testing.T) {
	svc := &fakeIAMService{} // default resolve → NoSuchEntity
	in := &ec2.AssociateIamInstanceProfileInput{
		InstanceId:         aws.String("i-001"),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String("ghost")},
	}
	_, err := AssociateIamInstanceProfile(in, nil, svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIamInstanceProfileNotFound, err.Error())
}

func TestAssociateIamInstanceProfile_PassRoleDenied(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileWithRole(), nil
	}}
	in := &ec2.AssociateIamInstanceProfileInput{
		InstanceId:         aws.String("i-001"),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
	}
	check := func(string) error { return errors.New(awserrors.ErrorAccessDenied) }
	_, err := AssociateIamInstanceProfile(in, nil, svc, testGwAccountID, check)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestAssociateIamInstanceProfile_NoResponders(t *testing.T) {
	_, nc := startTestNATSServer(t)
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileNoRole(), nil
	}}
	in := &ec2.AssociateIamInstanceProfileInput{
		InstanceId:         aws.String("i-no-daemon"),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameOther)},
	}
	// No subscriber on ec2.cmd.* → ErrNoResponders → maps to
	// InvalidInstanceID.NotFound so callers get an AWS-shaped 400 instead of
	// a raw NATS timeout.
	_, err := AssociateIamInstanceProfile(in, nc, svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidInstanceIDNotFound, err.Error())
}

func TestAssociateIamInstanceProfile_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileWithRole(), nil
	}}
	const instanceID = "i-assoc-success"
	const newAssocID = "iip-assoc-newly-generated01"
	ts := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)

	_, err := nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		var cmd spxtypes.EC2InstanceCommand
		require.NoError(t, json.Unmarshal(msg.Data, &cmd))
		assert.True(t, cmd.Attributes.AssociateIamInstanceProfile, "daemon must see the Associate flag set")
		require.NotNil(t, cmd.IamProfileAssociationData)
		assert.Equal(t, testProfileARNApp, cmd.IamProfileAssociationData.InstanceProfileArn,
			"daemon must receive the resolved canonical ARN, not the original name")

		resp, _ := json.Marshal(&ec2.IamInstanceProfileAssociation{
			AssociationId:      aws.String(newAssocID),
			InstanceId:         aws.String(instanceID),
			IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)},
			State:              aws.String(ec2.IamInstanceProfileAssociationStateAssociated),
			Timestamp:          aws.Time(ts),
		})
		msg.Respond(resp)
	})
	require.NoError(t, err)

	out, err := AssociateIamInstanceProfile(&ec2.AssociateIamInstanceProfileInput{
		InstanceId:         aws.String(instanceID),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
	}, nc, svc, testGwAccountID, func(string) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.IamInstanceProfileAssociation)
	assoc := out.IamInstanceProfileAssociation
	assert.Equal(t, newAssocID, aws.StringValue(assoc.AssociationId))
	assert.Equal(t, instanceID, aws.StringValue(assoc.InstanceId))
	require.NotNil(t, assoc.IamInstanceProfile)
	assert.Equal(t, testProfileARNApp, aws.StringValue(assoc.IamInstanceProfile.Arn))
	assert.Equal(t, testProfileIDApp, aws.StringValue(assoc.IamInstanceProfile.Id),
		"gateway must enrich the response with InstanceProfileID — daemons cannot resolve it")
	assert.Equal(t, ec2.IamInstanceProfileAssociationStateAssociated, aws.StringValue(assoc.State))
}

func TestAssociateIamInstanceProfile_DaemonAlreadyAssociated(t *testing.T) {
	_, nc := startTestNATSServer(t)
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileNoRole(), nil
	}}
	const instanceID = "i-already-bound"
	_, err := nc.Subscribe("ec2.cmd."+instanceID, func(msg *nats.Msg) {
		msg.Respond(utils.GenerateErrorPayload(awserrors.ErrorIamInstanceProfileAlreadyAssociated))
	})
	require.NoError(t, err)

	_, err = AssociateIamInstanceProfile(&ec2.AssociateIamInstanceProfileInput{
		InstanceId:         aws.String(instanceID),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameOther)},
	}, nc, svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorIamInstanceProfileAlreadyAssociated, err.Error())
}

// --- DisassociateIamInstanceProfile -----------------------------------------

func TestDisassociateIamInstanceProfile_MissingAssociationID(t *testing.T) {
	_, err := DisassociateIamInstanceProfile(&ec2.DisassociateIamInstanceProfileInput{}, nil, 0, testGwAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDisassociateIamInstanceProfile_NilInput(t *testing.T) {
	_, err := DisassociateIamInstanceProfile(nil, nil, 0, testGwAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestDisassociateIamInstanceProfile_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)
	const assocID = "iip-assoc-disassoc00000001"
	const instanceID = "i-dis-success"
	ts := time.Date(2026, 5, 27, 11, 0, 0, 0, time.UTC)

	// Two daemons subscribe; only the owner returns a populated record. The
	// null reply from the other daemon advances the expectedNodes collector so
	// we don't wait for the 3s deadline.
	_, err := nc.Subscribe("ec2.IamProfileAssociation.disassociate", func(msg *nats.Msg) {
		var req ec2.DisassociateIamInstanceProfileInput
		require.NoError(t, json.Unmarshal(msg.Data, &req))
		assert.Equal(t, assocID, aws.StringValue(req.AssociationId))
		resp, _ := json.Marshal(&ec2.IamInstanceProfileAssociation{
			AssociationId:      aws.String(assocID),
			InstanceId:         aws.String(instanceID),
			IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)},
			State:              aws.String(ec2.IamInstanceProfileAssociationStateDisassociating),
			Timestamp:          aws.Time(ts),
		})
		nc.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)

	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()
	_, err = nc2.Subscribe("ec2.IamProfileAssociation.disassociate", func(msg *nats.Msg) {
		nc2.Publish(msg.Reply, []byte("null"))
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	require.NoError(t, nc2.Flush())

	out, err := DisassociateIamInstanceProfile(&ec2.DisassociateIamInstanceProfileInput{
		AssociationId: aws.String(assocID),
	}, nc, 2, testGwAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.IamInstanceProfileAssociation)
	a := out.IamInstanceProfileAssociation
	assert.Equal(t, assocID, aws.StringValue(a.AssociationId))
	assert.Equal(t, instanceID, aws.StringValue(a.InstanceId))
	assert.Equal(t, testProfileARNApp, aws.StringValue(a.IamInstanceProfile.Arn))
	assert.Equal(t, ec2.IamInstanceProfileAssociationStateDisassociating, aws.StringValue(a.State),
		"AWS surfaces disassociating in the response so callers can show in-flight state")
}

func TestDisassociateIamInstanceProfile_NoSuchAssociation(t *testing.T) {
	_, nc := startTestNATSServer(t)
	// Every daemon NoOps with JSON null — gateway must surface
	// NoSuchAssociation, not timeout.
	_, err := nc.Subscribe("ec2.IamProfileAssociation.disassociate", func(msg *nats.Msg) {
		nc.Publish(msg.Reply, []byte("null"))
	})
	require.NoError(t, err)

	_, err = DisassociateIamInstanceProfile(&ec2.DisassociateIamInstanceProfileInput{
		AssociationId: aws.String("iip-assoc-stale-id"),
	}, nc, 1, testGwAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNoSuchAssociation, err.Error())
}

// --- ReplaceIamInstanceProfileAssociation -----------------------------------

func TestReplaceIamInstanceProfileAssociation_NilInput(t *testing.T) {
	_, err := ReplaceIamInstanceProfileAssociation(nil, nil, &fakeIAMService{}, 0, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestReplaceIamInstanceProfileAssociation_MissingAssociationID(t *testing.T) {
	in := &ec2.ReplaceIamInstanceProfileAssociationInput{
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
	}
	_, err := ReplaceIamInstanceProfileAssociation(in, nil, &fakeIAMService{}, 0, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestReplaceIamInstanceProfileAssociation_MissingProfileSpec(t *testing.T) {
	in := &ec2.ReplaceIamInstanceProfileAssociationInput{AssociationId: aws.String("iip-assoc-x")}
	_, err := ReplaceIamInstanceProfileAssociation(in, nil, &fakeIAMService{}, 0, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestReplaceIamInstanceProfileAssociation_PassRoleDeniedBeforeBroadcast(t *testing.T) {
	// nc=nil proves the call is rejected before any NATS dispatch.
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileWithRole(), nil
	}}
	check := func(string) error { return errors.New(awserrors.ErrorAccessDenied) }
	_, err := ReplaceIamInstanceProfileAssociation(&ec2.ReplaceIamInstanceProfileAssociationInput{
		AssociationId:      aws.String("iip-assoc-old"),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
	}, nil, svc, 0, testGwAccountID, check)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestReplaceIamInstanceProfileAssociation_Success(t *testing.T) {
	_, nc := startTestNATSServer(t)
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileWithRole(), nil
	}}
	const oldID = "iip-assoc-old00000000001"
	const newID = "iip-assoc-fresh000000001"
	const instanceID = "i-replace-target"

	_, err := nc.Subscribe("ec2.IamProfileAssociation.replace", func(msg *nats.Msg) {
		var req ec2.ReplaceIamInstanceProfileAssociationInput
		require.NoError(t, json.Unmarshal(msg.Data, &req))
		assert.Equal(t, oldID, aws.StringValue(req.AssociationId))
		require.NotNil(t, req.IamInstanceProfile)
		assert.Equal(t, testProfileARNApp, aws.StringValue(req.IamInstanceProfile.Arn),
			"daemon must receive resolved ARN, not the original name")
		assert.Nil(t, req.IamInstanceProfile.Name, "Name must not be carried over the wire — gateway normalises to ARN")
		resp, _ := json.Marshal(&ec2.IamInstanceProfileAssociation{
			AssociationId:      aws.String(newID),
			InstanceId:         aws.String(instanceID),
			IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)},
			State:              aws.String(ec2.IamInstanceProfileAssociationStateAssociated),
		})
		nc.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)

	out, err := ReplaceIamInstanceProfileAssociation(&ec2.ReplaceIamInstanceProfileAssociationInput{
		AssociationId:      aws.String(oldID),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)},
	}, nc, svc, 1, testGwAccountID, func(string) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, out)
	require.NotNil(t, out.IamInstanceProfileAssociation)
	a := out.IamInstanceProfileAssociation
	assert.Equal(t, newID, aws.StringValue(a.AssociationId), "Replace must surface the freshly generated ID")
	assert.NotEqual(t, oldID, aws.StringValue(a.AssociationId))
	assert.Equal(t, testProfileIDApp, aws.StringValue(a.IamInstanceProfile.Id))
}

func TestReplaceIamInstanceProfileAssociation_NoSuchAssociation(t *testing.T) {
	_, nc := startTestNATSServer(t)
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileNoRole(), nil
	}}
	_, err := nc.Subscribe("ec2.IamProfileAssociation.replace", func(msg *nats.Msg) {
		nc.Publish(msg.Reply, []byte("null"))
	})
	require.NoError(t, err)

	_, err = ReplaceIamInstanceProfileAssociation(&ec2.ReplaceIamInstanceProfileAssociationInput{
		AssociationId:      aws.String("iip-assoc-stale"),
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameOther)},
	}, nc, svc, 1, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorNoSuchAssociation, err.Error())
}

// --- DescribeIamInstanceProfileAssociations ---------------------------------

func TestDescribeIamInstanceProfileAssociations_FanOutAggregates(t *testing.T) {
	_, nc := startTestNATSServer(t)
	// Two daemons each return one record; the gateway must concatenate them.
	_, err := nc.Subscribe("ec2.IamProfileAssociation.describe", func(msg *nats.Msg) {
		resp, _ := json.Marshal(&ec2.DescribeIamInstanceProfileAssociationsOutput{
			IamInstanceProfileAssociations: []*ec2.IamInstanceProfileAssociation{{
				AssociationId:      aws.String("iip-assoc-node1-rec-001"),
				InstanceId:         aws.String("i-n1"),
				IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)},
				State:              aws.String(ec2.IamInstanceProfileAssociationStateAssociated),
			}},
		})
		nc.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)

	nc2, err := nats.Connect(nc.ConnectedUrl())
	require.NoError(t, err)
	defer nc2.Close()
	_, err = nc2.Subscribe("ec2.IamProfileAssociation.describe", func(msg *nats.Msg) {
		resp, _ := json.Marshal(&ec2.DescribeIamInstanceProfileAssociationsOutput{
			IamInstanceProfileAssociations: []*ec2.IamInstanceProfileAssociation{{
				AssociationId:      aws.String("iip-assoc-node2-rec-002"),
				InstanceId:         aws.String("i-n2"),
				IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNOther)},
				State:              aws.String(ec2.IamInstanceProfileAssociationStateAssociated),
			}},
		})
		nc2.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)
	require.NoError(t, nc.Flush())
	require.NoError(t, nc2.Flush())

	out, err := DescribeIamInstanceProfileAssociations(
		&ec2.DescribeIamInstanceProfileAssociationsInput{}, nc, 2, testGwAccountID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Len(t, out.IamInstanceProfileAssociations, 2)
}

func TestDescribeIamInstanceProfileAssociations_ForwardsFilters(t *testing.T) {
	_, nc := startTestNATSServer(t)
	var gotInput ec2.DescribeIamInstanceProfileAssociationsInput
	_, err := nc.Subscribe("ec2.IamProfileAssociation.describe", func(msg *nats.Msg) {
		_ = json.Unmarshal(msg.Data, &gotInput)
		resp, _ := json.Marshal(&ec2.DescribeIamInstanceProfileAssociationsOutput{})
		nc.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)

	_, err = DescribeIamInstanceProfileAssociations(&ec2.DescribeIamInstanceProfileAssociationsInput{
		AssociationIds: []*string{aws.String("iip-assoc-1"), aws.String("iip-assoc-2")},
		Filters: []*ec2.Filter{
			{Name: aws.String("instance-id"), Values: []*string{aws.String("i-001"), aws.String("i-002")}},
			{Name: aws.String("state"), Values: []*string{aws.String("associated")}},
		},
	}, nc, 1, testGwAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"iip-assoc-1", "iip-assoc-2"}, aws.StringValueSlice(gotInput.AssociationIds))
	require.Len(t, gotInput.Filters, 2)
	assert.Equal(t, "instance-id", aws.StringValue(gotInput.Filters[0].Name))
	assert.Equal(t, []string{"i-001", "i-002"}, aws.StringValueSlice(gotInput.Filters[0].Values))
	assert.Equal(t, "state", aws.StringValue(gotInput.Filters[1].Name))
}

func TestDescribeIamInstanceProfileAssociations_InvalidFilterName(t *testing.T) {
	_, nc := startTestNATSServer(t)
	_, err := DescribeIamInstanceProfileAssociations(&ec2.DescribeIamInstanceProfileAssociationsInput{
		Filters: []*ec2.Filter{{Name: aws.String("not-a-valid-filter")}},
	}, nc, 1, testGwAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func TestDescribeIamInstanceProfileAssociations_EmptyResultIsValid(t *testing.T) {
	_, nc := startTestNATSServer(t)
	_, err := nc.Subscribe("ec2.IamProfileAssociation.describe", func(msg *nats.Msg) {
		resp, _ := json.Marshal(&ec2.DescribeIamInstanceProfileAssociationsOutput{})
		nc.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)

	out, err := DescribeIamInstanceProfileAssociations(
		&ec2.DescribeIamInstanceProfileAssociationsInput{}, nc, 1, testGwAccountID)
	require.NoError(t, err, "no records is a valid response, not an error")
	assert.Empty(t, out.IamInstanceProfileAssociations)
}

// --- CountInstanceProfileAssociations ---------------------------------------

func TestCountInstanceProfileAssociations_MatchesByARN(t *testing.T) {
	_, nc := startTestNATSServer(t)
	_, err := nc.Subscribe("ec2.IamProfileAssociation.describe", func(msg *nats.Msg) {
		resp, _ := json.Marshal(&ec2.DescribeIamInstanceProfileAssociationsOutput{
			IamInstanceProfileAssociations: []*ec2.IamInstanceProfileAssociation{
				{AssociationId: aws.String("iip-assoc-001"), InstanceId: aws.String("i-1"), IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)}, State: aws.String("associated")},
				{AssociationId: aws.String("iip-assoc-002"), InstanceId: aws.String("i-2"), IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNOther)}, State: aws.String("associated")},
				{AssociationId: aws.String("iip-assoc-003"), InstanceId: aws.String("i-3"), IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)}, State: aws.String("associated")},
			},
		})
		nc.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)

	got, err := CountInstanceProfileAssociations(nc, 1, testGwAccountID, testProfileARNApp)
	require.NoError(t, err)
	assert.Equal(t, 2, got, "only associations whose ARN matches must count")
}

func TestCountInstanceProfileAssociations_NoMatches(t *testing.T) {
	_, nc := startTestNATSServer(t)
	_, err := nc.Subscribe("ec2.IamProfileAssociation.describe", func(msg *nats.Msg) {
		resp, _ := json.Marshal(&ec2.DescribeIamInstanceProfileAssociationsOutput{
			IamInstanceProfileAssociations: []*ec2.IamInstanceProfileAssociation{
				{AssociationId: aws.String("iip-assoc-001"), InstanceId: aws.String("i-1"), IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNOther)}, State: aws.String("associated")},
			},
		})
		nc.Publish(msg.Reply, resp)
	})
	require.NoError(t, err)
	got, err := CountInstanceProfileAssociations(nc, 1, testGwAccountID, testProfileARNApp)
	require.NoError(t, err)
	assert.Equal(t, 0, got)
}

// --- Association ID format sanity check (cross-package contract) ------------

// TestAssociationIDFormat asserts the AWS-compatible iip-assoc- prefix on IDs
// generated by GenerateResourceID. The format is a contract between the daemon
// (which produces it) and clients (who parse it via SDKs); regressions in the
// utils generator would break replay parsing of Disassociate/Replace.
func TestAssociationIDFormat(t *testing.T) {
	id := utils.GenerateResourceID("iip-assoc")
	assert.True(t, strings.HasPrefix(id, "iip-assoc-"),
		"GenerateResourceID must preserve the AWS-style hyphen between prefix and suffix")
	suffix := strings.TrimPrefix(id, "iip-assoc-")
	assert.NotEmpty(t, suffix, "association ID must carry a non-empty random suffix")
}
