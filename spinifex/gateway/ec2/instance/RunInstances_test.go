package gateway_ec2_instance

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_iam "github.com/mulgadc/spinifex/spinifex/handlers/iam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var defaults = ec2.RunInstancesInput{
	ImageId:      aws.String("ami-0abcdef1234567890"),
	InstanceType: aws.String("t2.micro"),
	MinCount:     aws.Int64(1),
	MaxCount:     aws.Int64(1),
	KeyName:      aws.String("my-key-pair"),
	SecurityGroupIds: []*string{
		aws.String("sg-0123456789abcdef0"),
	},
	SubnetId: aws.String("subnet-6e7f829e"),
}

func TestParseRunInstances(t *testing.T) {
	// Group multiple tests
	tests := []struct {
		name  string
		input *ec2.RunInstancesInput
		want  error
	}{

		{
			name: "InvalidMinCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(0),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "InvalidMaxCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(0),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "InvalidMinCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(0),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "InvalidNoImageId",
			input: &ec2.RunInstancesInput{
				ImageId:          aws.String(""),
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "InvalidNoInstanceType",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     aws.String(""),
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "InvalidNoInstanceType",
			input: &ec2.RunInstancesInput{
				ImageId:          aws.String("invalid-name-here"),
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidAMIIDMalformed),
		},

		{
			name:  "NilInput",
			input: nil,
			want:  errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "NilMinCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         nil,
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "MinCountGreaterThanMaxCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(5),
				MaxCount:         aws.Int64(2),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},

		{
			name: "NilImageId",
			input: &ec2.RunInstancesInput{
				ImageId:          nil,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "NilInstanceType",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     nil,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "NilMaxCount",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         nil,
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		{
			name: "MissingKeyName",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          nil,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: errors.New(awserrors.ErrorMissingParameter),
		},

		// Successful test
		{
			name: "ValidTest",
			input: &ec2.RunInstancesInput{
				ImageId:          defaults.ImageId,
				InstanceType:     defaults.InstanceType,
				MinCount:         aws.Int64(1),
				MaxCount:         aws.Int64(1),
				KeyName:          defaults.KeyName,
				SecurityGroupIds: defaults.SecurityGroupIds,
				SubnetId:         defaults.SubnetId,
			},
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Skip the valid test as it requires full daemon infrastructure
			// These tests are covered by the integration tests in service_nats_test.go
			if test.want == nil {
				t.Skip("Skipping valid test - requires full daemon infrastructure")
			}

			// For validation tests, we can pass nil conn since validation happens before NATS call
			response, err := RunInstances(test.input, nil, nil, "123456789012", nil)

			// Use assert to check if the error is as expected
			assert.Equal(t, test.want, err)

			if err != nil {
				assert.Len(t, response.Instances, 0)
			}
		})
	}

	// Additional test
}

// --- RunInstances + IamInstanceProfile -------------------------------------
//
// These tests exercise the gateway-side resolveAndAuthorizeInstanceProfile +
// enrichReservationWithProfileID flow without standing up the full daemon
// path. We exercise the helper directly so failures point at the IAM/policy
// integration, not at unrelated NATS plumbing.

func runValidInputWithProfile(spec *ec2.IamInstanceProfileSpecification) *ec2.RunInstancesInput {
	in := defaults
	in.IamInstanceProfile = spec
	return &in
}

func TestResolveAndAuthorizeInstanceProfile_Absent(t *testing.T) {
	// No IamInstanceProfile on the input → no resolution, no PassRole check.
	in := &ec2.RunInstancesInput{} // empty
	profile, err := resolveAndAuthorizeInstanceProfile(in, nil, testGwAccountID, nil)
	require.NoError(t, err)
	assert.Nil(t, profile)
}

func TestResolveAndAuthorizeInstanceProfile_NameForm_NormalisesToARN(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(_, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
		assert.Equal(t, testProfileNameApp, nameOrARN)
		return profileWithRole(), nil
	}}
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)})

	profile, err := resolveAndAuthorizeInstanceProfile(in, svc, testGwAccountID, func(string) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, testProfileARNApp, profile.ARN)

	// Daemons only ever see the canonical ARN — Name must be cleared post-resolution.
	require.NotNil(t, in.IamInstanceProfile)
	assert.Equal(t, testProfileARNApp, aws.StringValue(in.IamInstanceProfile.Arn))
	assert.Nil(t, in.IamInstanceProfile.Name, "Name must be cleared so the daemon cannot double-resolve")
}

func TestResolveAndAuthorizeInstanceProfile_ArnForm(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(_, nameOrARN string) (*handlers_iam.InstanceProfile, error) {
		assert.Equal(t, testProfileARNApp, nameOrARN)
		return profileWithRole(), nil
	}}
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{Arn: aws.String(testProfileARNApp)})
	profile, err := resolveAndAuthorizeInstanceProfile(in, svc, testGwAccountID, func(string) error { return nil })
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.Equal(t, testProfileARNApp, profile.ARN)
}

func TestResolveAndAuthorizeInstanceProfile_MissingNameAndArn(t *testing.T) {
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{})
	_, err := resolveAndAuthorizeInstanceProfile(in, &fakeIAMService{}, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorMissingParameter, err.Error())
}

func TestResolveAndAuthorizeInstanceProfile_NilIAMService(t *testing.T) {
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)})
	_, err := resolveAndAuthorizeInstanceProfile(in, nil, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
}

func TestResolveAndAuthorizeInstanceProfile_NotFoundMapsToEC2Error(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
	}}
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{Name: aws.String("ghost")})
	_, err := resolveAndAuthorizeInstanceProfile(in, svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidIamInstanceProfileNotFound, err.Error())
}

func TestResolveAndAuthorizeInstanceProfile_CrossAccountARNPassthrough(t *testing.T) {
	// IAM service rejects cross-account; gateway surfaces the AccessDenied verbatim.
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return nil, errors.New(awserrors.ErrorAccessDenied)
	}}
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{Arn: aws.String(testCrossAccountARN)})
	_, err := resolveAndAuthorizeInstanceProfile(in, svc, testGwAccountID, nil)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestResolveAndAuthorizeInstanceProfile_PassRoleDenied(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileWithRole(), nil
	}}
	check := func(roleARN string) error {
		assert.Equal(t, testRoleARNApp, roleARN)
		return errors.New(awserrors.ErrorAccessDenied)
	}
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameApp)})
	_, err := resolveAndAuthorizeInstanceProfile(in, svc, testGwAccountID, check)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorAccessDenied, err.Error())
}

func TestResolveAndAuthorizeInstanceProfile_NoRoleSkipsPassRole(t *testing.T) {
	svc := &fakeIAMService{resolveFn: func(string, string) (*handlers_iam.InstanceProfile, error) {
		return profileNoRole(), nil
	}}
	checkCalled := false
	check := func(string) error {
		checkCalled = true
		return errors.New(awserrors.ErrorAccessDenied)
	}
	in := runValidInputWithProfile(&ec2.IamInstanceProfileSpecification{Name: aws.String(testProfileNameOther)})
	profile, err := resolveAndAuthorizeInstanceProfile(in, svc, testGwAccountID, check)
	require.NoError(t, err)
	require.NotNil(t, profile)
	assert.False(t, checkCalled, "PassRole must be skipped when the profile has no role attached")
}

func TestEnrichReservationWithProfileID_PopulatesID(t *testing.T) {
	r := &ec2.Reservation{Instances: []*ec2.Instance{
		{InstanceId: aws.String("i-001"), IamInstanceProfile: &ec2.IamInstanceProfile{Arn: aws.String(testProfileARNApp)}},
		{InstanceId: aws.String("i-002")}, // no IamInstanceProfile yet — gateway fills both Arn + Id
	}}
	enrichReservationWithProfileID(r, profileWithRole())
	require.Len(t, r.Instances, 2)
	for _, inst := range r.Instances {
		require.NotNil(t, inst.IamInstanceProfile)
		assert.Equal(t, testProfileARNApp, aws.StringValue(inst.IamInstanceProfile.Arn))
		assert.Equal(t, testProfileIDApp, aws.StringValue(inst.IamInstanceProfile.Id))
	}
}

func TestEnrichReservationWithProfileID_NoProfileIsNoOp(t *testing.T) {
	r := &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String("i-001")}}}
	enrichReservationWithProfileID(r, nil)
	assert.Nil(t, r.Instances[0].IamInstanceProfile, "nil profile must not synthesise an empty IamInstanceProfile")
}

func TestEnrichReservationWithProfileID_NilReservationIsSafe(t *testing.T) {
	enrichReservationWithProfileID(nil, profileWithRole()) // must not panic
}
