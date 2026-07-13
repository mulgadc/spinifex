package handlers_ecs

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubImages returns a single spinifex-ecs-node AMI; nil images yields none.
type stubImages struct {
	images []*ec2.Image
}

func (s *stubImages) DescribeImages(_ context.Context, _ *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: s.images}, nil
}

// filteringStubImages applies its Filters against each image's tags, mirroring
// AWS's server-side tag filtering. Needed wherever both a plain and a GPU AMI
// are present, since the plain stubImages ignores Filters entirely and
// cannot distinguish which lookup (plain vs GPU) should win.
type filteringStubImages struct {
	images []*ec2.Image
}

func (s *filteringStubImages) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ string) (*ec2.DescribeImagesOutput, error) {
	out := &ec2.DescribeImagesOutput{}
	for _, img := range s.images {
		if imageMatchesFilters(img, in.Filters) {
			out.Images = append(out.Images, img)
		}
	}
	return out, nil
}

func imageMatchesFilters(img *ec2.Image, filters []*ec2.Filter) bool {
	tagVals := map[string]string{}
	for _, t := range img.Tags {
		tagVals[aws.StringValue(t.Key)] = aws.StringValue(t.Value)
	}
	for _, f := range filters {
		name := aws.StringValue(f.Name)
		const tagPrefix = "tag:"
		if len(name) <= len(tagPrefix) || name[:len(tagPrefix)] != tagPrefix {
			continue
		}
		key := name[len(tagPrefix):]
		val, ok := tagVals[key]
		if !ok {
			return false
		}
		matched := slices.Contains(aws.StringValueSlice(f.Values), val)
		if !matched {
			return false
		}
	}
	return true
}

// stubIAM converges to find-or-create: Get* report NoSuchEntity, Create* succeed.
type stubIAM struct{}

var (
	_ ecsIAM           = stubIAM{}
	_ ecsImageResolver = (*stubImages)(nil)
	_ ecsImageResolver = (*filteringStubImages)(nil)
)

func (stubIAM) GetRole(_ string, _ *iam.GetRoleInput) (*iam.GetRoleOutput, error) {
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (stubIAM) CreateRole(_ string, _ *iam.CreateRoleInput) (*iam.CreateRoleOutput, error) {
	return &iam.CreateRoleOutput{Role: &iam.Role{RoleName: aws.String(ecsInstanceRoleName)}}, nil
}

func (stubIAM) CreatePolicy(accountID string, _ *iam.CreatePolicyInput) (*iam.CreatePolicyOutput, error) {
	arn := "arn:aws:iam::" + accountID + ":policy/" + ecsInstanceRolePolicyName
	return &iam.CreatePolicyOutput{Policy: &iam.Policy{Arn: aws.String(arn)}}, nil
}

func (stubIAM) AttachRolePolicy(_ string, _ *iam.AttachRolePolicyInput) (*iam.AttachRolePolicyOutput, error) {
	return &iam.AttachRolePolicyOutput{}, nil
}

func (stubIAM) GetInstanceProfile(_ string, _ *iam.GetInstanceProfileInput) (*iam.GetInstanceProfileOutput, error) {
	return nil, errors.New(awserrors.ErrorIAMNoSuchEntity)
}

func (stubIAM) CreateInstanceProfile(accountID string, _ *iam.CreateInstanceProfileInput) (*iam.CreateInstanceProfileOutput, error) {
	arn := "arn:aws:iam::" + accountID + ":instance-profile/" + ecsInstanceRoleName
	return &iam.CreateInstanceProfileOutput{InstanceProfile: &iam.InstanceProfile{Arn: aws.String(arn)}}, nil
}

func (stubIAM) AddRoleToInstanceProfile(_ string, _ *iam.AddRoleToInstanceProfileInput) (*iam.AddRoleToInstanceProfileOutput, error) {
	return &iam.AddRoleToInstanceProfileOutput{}, nil
}

func ecsNodeImage() []*ec2.Image {
	return []*ec2.Image{{
		ImageId:      aws.String("ami-ecs"),
		CreationDate: aws.String("2026-01-01T00:00:00.000Z"),
		Tags: []*ec2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByECS)},
		},
	}}
}

func TestProvisionCapacity_BuildsRunInstancesInput(t *testing.T) {
	var captured *ec2.RunInstancesInput
	svc := NewService(nil, testRegion, "internal").WithDeps(Deps{
		GatewayBaseURL: "https://10.0.0.1:9999",
		GatewayCACert:  "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----",
		IAM:            stubIAM{},
		Images:         &stubImages{images: ecsNodeImage()},
		RunInstances: func(_ context.Context, in *ec2.RunInstancesInput, _ string) (*ec2.Reservation, error) {
			captured = in
			return &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String("i-123")}}}, nil
		},
	})

	out, err := svc.ProvisionCapacity(context.Background(), &ProvisionCapacityInput{
		Cluster:         "web",
		InstanceType:    "t3.medium",
		Count:           2,
		SubnetID:        "subnet-1",
		SecurityGroupID: "sg-1",
		KeyName:         "kp-1",
	}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, []string{"i-123"}, out.InstanceIDs)

	require.NotNil(t, captured)
	assert.Equal(t, "ami-ecs", aws.StringValue(captured.ImageId))
	assert.Equal(t, "t3.medium", aws.StringValue(captured.InstanceType))
	assert.Equal(t, int64(2), aws.Int64Value(captured.MinCount))
	assert.Equal(t, int64(2), aws.Int64Value(captured.MaxCount))
	assert.Equal(t, "subnet-1", aws.StringValue(captured.SubnetId))
	assert.Equal(t, []string{"sg-1"}, aws.StringValueSlice(captured.SecurityGroupIds))
	assert.Equal(t, "kp-1", aws.StringValue(captured.KeyName))

	require.NotNil(t, captured.IamInstanceProfile)
	assert.NotEmpty(t, aws.StringValue(captured.IamInstanceProfile.Arn))

	require.NotNil(t, captured.UserData)
	assert.Contains(t, aws.StringValue(captured.UserData), "ECS_CLUSTER=web")
	assert.NotContains(t, aws.StringValue(captured.UserData), "ECS_ACCESS_KEY")
}

func TestProvisionCapacity_DefaultsAndCount(t *testing.T) {
	var captured *ec2.RunInstancesInput
	svc := NewService(nil, testRegion, "internal").WithDeps(Deps{
		IAM:    stubIAM{},
		Images: &stubImages{images: ecsNodeImage()},
		RunInstances: func(_ context.Context, in *ec2.RunInstancesInput, _ string) (*ec2.Reservation, error) {
			captured = in
			return &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String("i-1")}}}, nil
		},
	})

	_, err := svc.ProvisionCapacity(context.Background(), &ProvisionCapacityInput{
		Cluster:         "web",
		SubnetID:        "subnet-1",
		SecurityGroupID: "sg-1",
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Equal(t, defaultCapacityInstanceType, aws.StringValue(captured.InstanceType))
	assert.Equal(t, int64(1), aws.Int64Value(captured.MinCount))
	assert.Nil(t, captured.KeyName)
}

func TestProvisionCapacity_MissingRequired(t *testing.T) {
	svc := NewService(nil, testRegion, "internal").WithDeps(Deps{
		IAM: stubIAM{}, Images: &stubImages{images: ecsNodeImage()},
		RunInstances: func(context.Context, *ec2.RunInstancesInput, string) (*ec2.Reservation, error) { return nil, nil },
	})
	_, err := svc.ProvisionCapacity(context.Background(), &ProvisionCapacityInput{Cluster: "web"}, testAccountID)
	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorInvalidParameterValue, err.Error())
}

func ecsGPUNodeImage(id, created string) *ec2.Image {
	return &ec2.Image{
		ImageId:      aws.String(id),
		CreationDate: aws.String(created),
		Tags: []*ec2.Tag{
			{Key: aws.String(tags.ManagedByKey), Value: aws.String(tags.ManagedByECS)},
			{Key: aws.String(tags.GPUVendorKey), Value: aws.String(tags.GPUVendorNVIDIA)},
		},
	}
}

func TestLookupECSGPUNodeAMI_SelectsGPUTaggedAMI(t *testing.T) {
	images := &stubImages{images: []*ec2.Image{ecsGPUNodeImage("ami-ecs-gpu", "2026-06-01T00:00:00.000Z")}}

	got, err := lookupECSGPUNodeAMI(context.Background(), images, testAccountID, tags.GPUVendorNVIDIA)
	require.NoError(t, err)
	assert.Equal(t, "ami-ecs-gpu", got)
}

func TestLookupECSGPUNodeAMI_NoGPUAMINoFallback(t *testing.T) {
	images := &stubImages{images: nil}

	_, err := lookupECSGPUNodeAMI(context.Background(), images, testAccountID, tags.GPUVendorNVIDIA)
	require.ErrorIs(t, err, ErrECSGPUNodeAMINotFound)
	assert.Contains(t, err.Error(), "gpu-vendor=nvidia")
}

func TestLookupECSGPUNodeAMI_NewestCreationDateWins(t *testing.T) {
	images := &stubImages{images: []*ec2.Image{
		ecsGPUNodeImage("ami-ecs-gpu-old", "2026-01-01T00:00:00.000Z"),
		ecsGPUNodeImage("ami-ecs-gpu-new", "2026-06-01T00:00:00.000Z"),
	}}

	got, err := lookupECSGPUNodeAMI(context.Background(), images, testAccountID, tags.GPUVendorNVIDIA)
	require.NoError(t, err)
	assert.Equal(t, "ami-ecs-gpu-new", got)
}

func TestLookupECSNodeAMI_ExcludesGPUTaggedAMI(t *testing.T) {
	// The GPU node AMI also carries managed-by=ecs, so a managed-by DescribeImages
	// returns it too (stubImages ignores filters, mirroring that). Even when it is
	// newer, the plain lookup must skip it so ordinary container instances do not
	// boot the driverless-mismatch GPU image.
	images := &stubImages{images: []*ec2.Image{
		ecsNodeImage()[0],
		ecsGPUNodeImage("ami-ecs-gpu-newer", "2026-06-01T00:00:00.000Z"),
	}}

	got, err := lookupECSNodeAMI(context.Background(), images, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, "ami-ecs", got, "newer gpu-vendor-tagged AMI must not hijack the plain lookup")
}

func TestProvisionCapacity_GPUInstanceTypeUsesGPUAMI(t *testing.T) {
	var captured *ec2.RunInstancesInput
	svc := NewService(nil, testRegion, "internal").WithDeps(Deps{
		IAM:    stubIAM{},
		Images: &filteringStubImages{images: []*ec2.Image{ecsGPUNodeImage("ami-ecs-gpu", "2026-06-01T00:00:00.000Z")}},
		RunInstances: func(_ context.Context, in *ec2.RunInstancesInput, _ string) (*ec2.Reservation, error) {
			captured = in
			return &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String("i-gpu-1")}}}, nil
		},
	})

	_, err := svc.ProvisionCapacity(context.Background(), &ProvisionCapacityInput{
		Cluster:         "web",
		InstanceType:    "g5.xlarge",
		SubnetID:        "subnet-1",
		SecurityGroupID: "sg-1",
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Equal(t, "ami-ecs-gpu", aws.StringValue(captured.ImageId))
}

func TestProvisionCapacity_NonGPUInstanceTypeUsesPlainAMI(t *testing.T) {
	var captured *ec2.RunInstancesInput
	svc := NewService(nil, testRegion, "internal").WithDeps(Deps{
		IAM:    stubIAM{},
		Images: &filteringStubImages{images: ecsNodeImage()},
		RunInstances: func(_ context.Context, in *ec2.RunInstancesInput, _ string) (*ec2.Reservation, error) {
			captured = in
			return &ec2.Reservation{Instances: []*ec2.Instance{{InstanceId: aws.String("i-1")}}}, nil
		},
	})

	_, err := svc.ProvisionCapacity(context.Background(), &ProvisionCapacityInput{
		Cluster:         "web",
		InstanceType:    "t3.medium",
		SubnetID:        "subnet-1",
		SecurityGroupID: "sg-1",
	}, testAccountID)
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.Equal(t, "ami-ecs", aws.StringValue(captured.ImageId))
}

// GPU instance type must never fall back to the plain (non-GPU) AMI: with only
// the plain AMI resolvable, the gpu-vendor filter matches nothing and the GPU
// lookup must fail rather than silently reuse the driverless image.
func TestProvisionCapacity_GPUInstanceTypeNoGPUAMIErrors(t *testing.T) {
	svc := NewService(nil, testRegion, "internal").WithDeps(Deps{
		IAM:    stubIAM{},
		Images: &filteringStubImages{images: ecsNodeImage()}, // only the plain (non-GPU) AMI resolves
		RunInstances: func(context.Context, *ec2.RunInstancesInput, string) (*ec2.Reservation, error) {
			t.Fatal("RunInstances must not be called when no GPU AMI resolves")
			return nil, nil
		},
	})

	_, err := svc.ProvisionCapacity(context.Background(), &ProvisionCapacityInput{
		Cluster:         "web",
		InstanceType:    "g5.xlarge",
		SubnetID:        "subnet-1",
		SecurityGroupID: "sg-1",
	}, testAccountID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrECSGPUNodeAMINotFound, "GPU instance type must never fall back to the non-GPU AMI")
}

func TestProvisionCapacity_AMINotFound(t *testing.T) {
	svc := NewService(nil, testRegion, "internal").WithDeps(Deps{
		IAM:    stubIAM{},
		Images: &stubImages{images: nil},
		RunInstances: func(context.Context, *ec2.RunInstancesInput, string) (*ec2.Reservation, error) {
			t.Fatal("RunInstances must not be called when no AMI resolves")
			return nil, nil
		},
	})
	_, err := svc.ProvisionCapacity(context.Background(), &ProvisionCapacityInput{
		Cluster: "web", SubnetID: "subnet-1", SecurityGroupID: "sg-1",
	}, testAccountID)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrECSNodeAMINotFound)
}
