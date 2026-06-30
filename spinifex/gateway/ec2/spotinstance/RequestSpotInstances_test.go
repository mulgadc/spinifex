package gateway_ec2_spotinstance

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	handlers_ec2_spotinstance "github.com/mulgadc/spinifex/spinifex/handlers/ec2/spotinstance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validLaunchSpec() *ec2.RequestSpotLaunchSpecification {
	return &ec2.RequestSpotLaunchSpecification{
		ImageId:          aws.String("ami-0abcdef1234567890"),
		InstanceType:     aws.String("t2.micro"),
		KeyName:          aws.String("my-key-pair"),
		SubnetId:         aws.String("subnet-6e7f829e"),
		SecurityGroupIds: []*string{aws.String("sg-0123456789abcdef0")},
	}
}

func TestValidateRequestSpotInstancesInput(t *testing.T) {
	tests := []struct {
		name  string
		input *ec2.RequestSpotInstancesInput
		want  error
	}{
		{
			name:  "Valid",
			input: &ec2.RequestSpotInstancesInput{LaunchSpecification: validLaunchSpec()},
			want:  nil,
		},
		{
			name:  "NilInput",
			input: nil,
			want:  errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name:  "MissingLaunchSpecification",
			input: &ec2.RequestSpotInstancesInput{},
			want:  errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "MissingImageId",
			input: &ec2.RequestSpotInstancesInput{LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
				InstanceType: aws.String("t2.micro"),
				KeyName:      aws.String("my-key-pair"),
			}},
			want: errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "MalformedImageId",
			input: &ec2.RequestSpotInstancesInput{LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
				ImageId:      aws.String("invalid-name-here"),
				InstanceType: aws.String("t2.micro"),
				KeyName:      aws.String("my-key-pair"),
			}},
			want: errors.New(awserrors.ErrorInvalidAMIIDMalformed),
		},
		{
			name: "MissingInstanceType",
			input: &ec2.RequestSpotInstancesInput{LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
				ImageId: aws.String("ami-0abcdef1234567890"),
				KeyName: aws.String("my-key-pair"),
			}},
			want: errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "MissingKeyName",
			input: &ec2.RequestSpotInstancesInput{LaunchSpecification: &ec2.RequestSpotLaunchSpecification{
				ImageId:      aws.String("ami-0abcdef1234567890"),
				InstanceType: aws.String("t2.micro"),
			}},
			want: errors.New(awserrors.ErrorMissingParameter),
		},
		{
			name: "InstanceCountZero",
			input: &ec2.RequestSpotInstancesInput{
				LaunchSpecification: validLaunchSpec(),
				InstanceCount:       aws.Int64(0),
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},
		{
			name: "InstanceCountNegative",
			input: &ec2.RequestSpotInstancesInput{
				LaunchSpecification: validLaunchSpec(),
				InstanceCount:       aws.Int64(-1),
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},
		{
			name: "InvalidType",
			input: &ec2.RequestSpotInstancesInput{
				LaunchSpecification: validLaunchSpec(),
				Type:                aws.String("spot-block"),
			},
			want: errors.New(awserrors.ErrorInvalidParameterValue),
		},
		{
			name: "PersistentTypeAccepted",
			input: &ec2.RequestSpotInstancesInput{
				LaunchSpecification: validLaunchSpec(),
				Type:                aws.String(ec2.SpotInstanceTypePersistent),
			},
			want: nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, ValidateRequestSpotInstancesInput(test.input))
		})
	}
}

func TestSpotInstanceCountDefaultsToOne(t *testing.T) {
	assert.Equal(t, int64(1), spotInstanceCount(&ec2.RequestSpotInstancesInput{}))
	assert.Equal(t, int64(3), spotInstanceCount(&ec2.RequestSpotInstancesInput{InstanceCount: aws.Int64(3)}))
}

func TestRunInputFromLaunchSpec_CountAndClientToken(t *testing.T) {
	input := &ec2.RequestSpotInstancesInput{
		LaunchSpecification: validLaunchSpec(),
		ClientToken:         aws.String("idem-token-123"),
	}

	runInput := runInputFromLaunchSpec(input, 4)

	require.NotNil(t, runInput.MinCount)
	require.NotNil(t, runInput.MaxCount)
	assert.Equal(t, int64(4), *runInput.MinCount, "MinCount maps to InstanceCount")
	assert.Equal(t, int64(4), *runInput.MaxCount, "MaxCount maps to InstanceCount")
	assert.Equal(t, "idem-token-123", aws.StringValue(runInput.ClientToken), "ClientToken passes through")
}

func TestRunInputFromLaunchSpec_FieldTranslation(t *testing.T) {
	spec := validLaunchSpec()
	spec.UserData = aws.String("dXNlci1kYXRh")
	spec.Placement = &ec2.SpotPlacement{GroupName: aws.String("pg-spread")}
	spec.IamInstanceProfile = &ec2.IamInstanceProfileSpecification{Name: aws.String("worker-profile")}
	input := &ec2.RequestSpotInstancesInput{LaunchSpecification: spec}

	runInput := runInputFromLaunchSpec(input, 1)

	assert.Equal(t, "ami-0abcdef1234567890", aws.StringValue(runInput.ImageId))
	assert.Equal(t, "t2.micro", aws.StringValue(runInput.InstanceType))
	assert.Equal(t, "my-key-pair", aws.StringValue(runInput.KeyName))
	assert.Equal(t, "subnet-6e7f829e", aws.StringValue(runInput.SubnetId))
	assert.Equal(t, "dXNlci1kYXRh", aws.StringValue(runInput.UserData))
	assert.Equal(t, []*string{aws.String("sg-0123456789abcdef0")}, runInput.SecurityGroupIds)
	require.NotNil(t, runInput.Placement)
	assert.Equal(t, "pg-spread", aws.StringValue(runInput.Placement.GroupName))
	require.NotNil(t, runInput.IamInstanceProfile)
	assert.Equal(t, "worker-profile", aws.StringValue(runInput.IamInstanceProfile.Name))
}

// TestLaunchSpecRoundTrip verifies the spot launch spec survives translation
// into a RunInstancesInput and back into the read-side LaunchSpecification,
// including the security-group ID -> GroupIdentifier shape change.
func TestLaunchSpecRoundTrip(t *testing.T) {
	spec := validLaunchSpec()
	spec.UserData = aws.String("dXNlci1kYXRh")
	spec.Placement = &ec2.SpotPlacement{GroupName: aws.String("pg-cluster")}
	input := &ec2.RequestSpotInstancesInput{LaunchSpecification: spec}

	runInput := runInputFromLaunchSpec(input, 1)
	out := launchSpecFromRunInput(runInput)

	assert.Equal(t, "ami-0abcdef1234567890", aws.StringValue(out.ImageId))
	assert.Equal(t, "t2.micro", aws.StringValue(out.InstanceType))
	assert.Equal(t, "my-key-pair", aws.StringValue(out.KeyName))
	assert.Equal(t, "subnet-6e7f829e", aws.StringValue(out.SubnetId))
	assert.Equal(t, "dXNlci1kYXRh", aws.StringValue(out.UserData))
	require.Len(t, out.SecurityGroups, 1)
	assert.Equal(t, "sg-0123456789abcdef0", aws.StringValue(out.SecurityGroups[0].GroupId))
	require.NotNil(t, out.Placement)
	assert.Equal(t, "pg-cluster", aws.StringValue(out.Placement.GroupName))
}

func TestBuildSpotRequests_MapsInstancesAndDefaults(t *testing.T) {
	input := &ec2.RequestSpotInstancesInput{
		LaunchSpecification: validLaunchSpec(),
		InstanceCount:       aws.Int64(2),
		SpotPrice:           aws.String("0.05"),
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String(ec2.ResourceTypeSpotInstancesRequest),
			Tags:         []*ec2.Tag{{Key: aws.String("Name"), Value: aws.String("worker")}},
		}},
	}
	runInput := runInputFromLaunchSpec(input, 2)
	instances := []*ec2.Instance{
		{InstanceId: aws.String("i-aaa")},
		{InstanceId: aws.String("i-bbb")},
	}

	requests := buildSpotRequests(input, runInput, instances, "ap-southeast-2a")

	require.Len(t, requests, 2)
	for _, req := range requests {
		assert.Equal(t, ec2.SpotInstanceStateActive, aws.StringValue(req.State))
		assert.Equal(t, handlers_ec2_spotinstance.SpotStatusCodeFulfilled, aws.StringValue(req.Status.Code))
		assert.Equal(t, ec2.SpotInstanceTypeOneTime, aws.StringValue(req.Type), "Type defaults to one-time")
		assert.Equal(t, ec2.RIProductDescriptionLinuxUnix, aws.StringValue(req.ProductDescription))
		assert.Equal(t, "ap-southeast-2a", aws.StringValue(req.LaunchedAvailabilityZone))
		assert.Equal(t, "0.05", aws.StringValue(req.SpotPrice))
		assert.True(t, strings.HasPrefix(aws.StringValue(req.SpotInstanceRequestId), "sir-"))
		require.Len(t, req.Tags, 1)
		assert.Equal(t, "Name", aws.StringValue(req.Tags[0].Key))
	}
	assert.Equal(t, "i-aaa", aws.StringValue(requests[0].InstanceId))
	assert.Equal(t, "i-bbb", aws.StringValue(requests[1].InstanceId))
	assert.NotEqual(t, aws.StringValue(requests[0].SpotInstanceRequestId), aws.StringValue(requests[1].SpotInstanceRequestId), "each SIR gets a unique id")
}

func TestBuildSpotRequests_PersistentTypePreserved(t *testing.T) {
	input := &ec2.RequestSpotInstancesInput{
		LaunchSpecification: validLaunchSpec(),
		Type:                aws.String(ec2.SpotInstanceTypePersistent),
	}
	runInput := runInputFromLaunchSpec(input, 1)

	requests := buildSpotRequests(input, runInput, []*ec2.Instance{{InstanceId: aws.String("i-ccc")}}, "ap-southeast-2a")

	require.Len(t, requests, 1)
	assert.Equal(t, ec2.SpotInstanceTypePersistent, aws.StringValue(requests[0].Type))
}
