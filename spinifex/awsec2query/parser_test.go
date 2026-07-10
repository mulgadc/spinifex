package awsec2query

import (
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryParamsToStruct_RunInstances(t *testing.T) {
	args := map[string]string{
		"Action":       "RunInstances",
		"ImageId":      "ami-123456789",
		"MinCount":     "1",
		"MaxCount":     "2",
		"InstanceType": "t2.micro",
		"KeyName":      "my-key-pair",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, "ami-123456789", aws.StringValue(input.ImageId))
	assert.Equal(t, int64(1), aws.Int64Value(input.MinCount))
	assert.Equal(t, int64(2), aws.Int64Value(input.MaxCount))
	assert.Equal(t, "t2.micro", aws.StringValue(input.InstanceType))
	assert.Equal(t, "my-key-pair", aws.StringValue(input.KeyName))
}

func TestQueryParamsToStruct_RunInstancesWithSecurityGroups(t *testing.T) {
	args := map[string]string{
		"Action":            "RunInstances",
		"ImageId":           "ami-123456789",
		"MinCount":          "1",
		"MaxCount":          "1",
		"SecurityGroupId.1": "sg-12345678",
		"SecurityGroupId.2": "sg-87654321",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, "ami-123456789", aws.StringValue(input.ImageId))
	assert.Len(t, input.SecurityGroupIds, 2)
	assert.Equal(t, "sg-12345678", aws.StringValue(input.SecurityGroupIds[0]))
	assert.Equal(t, "sg-87654321", aws.StringValue(input.SecurityGroupIds[1]))
}

func TestQueryParamsToStruct_DescribeInstances(t *testing.T) {
	args := map[string]string{
		"Action":       "DescribeInstances",
		"InstanceId.1": "i-1234567890abcdef0",
		"InstanceId.2": "i-0987654321fedcba0",
		"MaxResults":   "100",
	}

	input := &ec2.DescribeInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.InstanceIds, 2)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.InstanceIds[0]))
	assert.Equal(t, "i-0987654321fedcba0", aws.StringValue(input.InstanceIds[1]))
	assert.Equal(t, int64(100), aws.Int64Value(input.MaxResults))
}

func TestQueryParamsToStruct_DescribeInstancesWithFilters(t *testing.T) {
	args := map[string]string{
		"Action":           "DescribeInstances",
		"Filter.1.Name":    "instance-type",
		"Filter.1.Value.1": "t2.micro",
		"Filter.1.Value.2": "t2.small",
		"Filter.2.Name":    "instance-state-name",
		"Filter.2.Value.1": "running",
	}

	input := &ec2.DescribeInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.Filters, 2)
	assert.Equal(t, "instance-type", aws.StringValue(input.Filters[0].Name))
	assert.Len(t, input.Filters[0].Values, 2)
	assert.Equal(t, "t2.micro", aws.StringValue(input.Filters[0].Values[0]))
	assert.Equal(t, "t2.small", aws.StringValue(input.Filters[0].Values[1]))
	assert.Equal(t, "instance-state-name", aws.StringValue(input.Filters[1].Name))
	assert.Len(t, input.Filters[1].Values, 1)
	assert.Equal(t, "running", aws.StringValue(input.Filters[1].Values[0]))
}

func TestQueryParamsToStruct_StopInstances(t *testing.T) {
	args := map[string]string{
		"Action":       "StopInstances",
		"InstanceId.1": "i-1234567890abcdef0",
		"InstanceId.2": "i-0987654321fedcba0",
		"Force":        "true",
	}

	input := &ec2.StopInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.InstanceIds, 2)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.InstanceIds[0]))
	assert.Equal(t, "i-0987654321fedcba0", aws.StringValue(input.InstanceIds[1]))
	assert.Equal(t, true, aws.BoolValue(input.Force))
}

func TestQueryParamsToStruct_StartInstances(t *testing.T) {
	args := map[string]string{
		"Action":       "StartInstances",
		"InstanceId.1": "i-1234567890abcdef0",
		"InstanceId.2": "i-0987654321fedcba0",
		"InstanceId.3": "i-abcdef1234567890",
	}

	input := &ec2.StartInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.InstanceIds, 3)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.InstanceIds[0]))
	assert.Equal(t, "i-0987654321fedcba0", aws.StringValue(input.InstanceIds[1]))
	assert.Equal(t, "i-abcdef1234567890", aws.StringValue(input.InstanceIds[2]))
}

func TestQueryParamsToStruct_RebootInstances(t *testing.T) {
	args := map[string]string{
		"Action":       "RebootInstances",
		"InstanceId.1": "i-1234567890abcdef0",
	}

	input := &ec2.RebootInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.InstanceIds, 1)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.InstanceIds[0]))
}

func TestQueryParamsToStruct_TerminateInstances(t *testing.T) {
	args := map[string]string{
		"Action":       "TerminateInstances",
		"InstanceId.1": "i-1234567890abcdef0",
		"InstanceId.2": "i-0987654321fedcba0",
	}

	input := &ec2.TerminateInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.InstanceIds, 2)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.InstanceIds[0]))
	assert.Equal(t, "i-0987654321fedcba0", aws.StringValue(input.InstanceIds[1]))
}

func TestQueryParamsToStruct_ComplexTagSpecifications(t *testing.T) {
	args := map[string]string{
		"Action":                          "RunInstances",
		"ImageId":                         "ami-123456789",
		"MinCount":                        "1",
		"MaxCount":                        "1",
		"TagSpecification.1.ResourceType": "instance",
		"TagSpecification.1.Tag.1.Key":    "Name",
		"TagSpecification.1.Tag.1.Value":  "MyInstance",
		"TagSpecification.1.Tag.2.Key":    "Environment",
		"TagSpecification.1.Tag.2.Value":  "Production",
		"TagSpecification.2.ResourceType": "volume",
		"TagSpecification.2.Tag.1.Key":    "VolumeType",
		"TagSpecification.2.Tag.1.Value":  "Root",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.TagSpecifications, 2)

	// First tag specification
	assert.Equal(t, "instance", aws.StringValue(input.TagSpecifications[0].ResourceType))
	assert.Len(t, input.TagSpecifications[0].Tags, 2)
	assert.Equal(t, "Name", aws.StringValue(input.TagSpecifications[0].Tags[0].Key))
	assert.Equal(t, "MyInstance", aws.StringValue(input.TagSpecifications[0].Tags[0].Value))
	assert.Equal(t, "Environment", aws.StringValue(input.TagSpecifications[0].Tags[1].Key))
	assert.Equal(t, "Production", aws.StringValue(input.TagSpecifications[0].Tags[1].Value))

	// Second tag specification
	assert.Equal(t, "volume", aws.StringValue(input.TagSpecifications[1].ResourceType))
	assert.Len(t, input.TagSpecifications[1].Tags, 1)
	assert.Equal(t, "VolumeType", aws.StringValue(input.TagSpecifications[1].Tags[0].Key))
	assert.Equal(t, "Root", aws.StringValue(input.TagSpecifications[1].Tags[0].Value))
}

func TestQueryParamsToStruct_InvalidInput(t *testing.T) {
	args := map[string]string{
		"Action": "RunInstances",
	}

	// Test with non-pointer
	input := ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must be a pointer")
}

func TestQueryParamsToStruct_EmptyParams(t *testing.T) {
	args := map[string]string{}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	// All fields should be nil/zero
	assert.Nil(t, input.ImageId)
	assert.Nil(t, input.MinCount)
}

func TestQueryParamsToStruct_BlockDeviceMappingsWithEBS(t *testing.T) {
	args := map[string]string{
		"Action":                              "RunInstances",
		"ImageId":                             "ami-0abcdef1234567890",
		"InstanceType":                        "t2.micro",
		"MinCount":                            "1",
		"MaxCount":                            "1",
		"BlockDeviceMapping.1.DeviceName":     "/dev/sdh",
		"BlockDeviceMapping.1.Ebs.VolumeSize": "100",
		"BlockDeviceMapping.1.Ebs.VolumeType": "gp3",
		"BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, "ami-0abcdef1234567890", aws.StringValue(input.ImageId))
	assert.Equal(t, "t2.micro", aws.StringValue(input.InstanceType))
	assert.Len(t, input.BlockDeviceMappings, 1)

	// Check first block device mapping
	bdm := input.BlockDeviceMappings[0]
	assert.Equal(t, "/dev/sdh", aws.StringValue(bdm.DeviceName))
	assert.NotNil(t, bdm.Ebs)
	assert.Equal(t, int64(100), aws.Int64Value(bdm.Ebs.VolumeSize))
	assert.Equal(t, "gp3", aws.StringValue(bdm.Ebs.VolumeType))
	assert.Equal(t, true, aws.BoolValue(bdm.Ebs.DeleteOnTermination))
}

func TestQueryParamsToStruct_BlockDeviceMappingsWithEphemeral(t *testing.T) {
	args := map[string]string{
		"Action":                           "RunInstances",
		"ImageId":                          "ami-0abcdef1234567890",
		"InstanceType":                     "m3.medium",
		"MinCount":                         "1",
		"MaxCount":                         "1",
		"BlockDeviceMapping.1.DeviceName":  "/dev/sdc",
		"BlockDeviceMapping.1.VirtualName": "ephemeral1",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.BlockDeviceMappings, 1)

	// Check ephemeral mapping
	bdm := input.BlockDeviceMappings[0]
	assert.Equal(t, "/dev/sdc", aws.StringValue(bdm.DeviceName))
	assert.Equal(t, "ephemeral1", aws.StringValue(bdm.VirtualName))
	assert.Nil(t, bdm.Ebs)
}

func TestQueryParamsToStruct_MultipleBlockDeviceMappings(t *testing.T) {
	args := map[string]string{
		"Action":       "RunInstances",
		"ImageId":      "ami-0abcdef1234567890",
		"InstanceType": "t2.micro",
		"MinCount":     "1",
		"MaxCount":     "1",
		// First mapping - EBS volume with snapshot
		"BlockDeviceMapping.1.DeviceName":              "/dev/sda1",
		"BlockDeviceMapping.1.Ebs.SnapshotId":          "snap-1234567890abcdef0",
		"BlockDeviceMapping.1.Ebs.VolumeSize":          "30",
		"BlockDeviceMapping.1.Ebs.VolumeType":          "gp3",
		"BlockDeviceMapping.1.Ebs.Encrypted":           "true",
		"BlockDeviceMapping.1.Ebs.DeleteOnTermination": "true",
		// Second mapping - Empty EBS volume
		"BlockDeviceMapping.2.DeviceName":     "/dev/sdh",
		"BlockDeviceMapping.2.Ebs.VolumeSize": "100",
		"BlockDeviceMapping.2.Ebs.VolumeType": "gp2",
		// Third mapping - Instance store
		"BlockDeviceMapping.3.DeviceName":  "/dev/sdc",
		"BlockDeviceMapping.3.VirtualName": "ephemeral0",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.BlockDeviceMappings, 3)

	// First mapping - EBS with snapshot
	bdm1 := input.BlockDeviceMappings[0]
	assert.Equal(t, "/dev/sda1", aws.StringValue(bdm1.DeviceName))
	assert.NotNil(t, bdm1.Ebs)
	assert.Equal(t, "snap-1234567890abcdef0", aws.StringValue(bdm1.Ebs.SnapshotId))
	assert.Equal(t, int64(30), aws.Int64Value(bdm1.Ebs.VolumeSize))
	assert.Equal(t, "gp3", aws.StringValue(bdm1.Ebs.VolumeType))
	assert.Equal(t, true, aws.BoolValue(bdm1.Ebs.Encrypted))
	assert.Equal(t, true, aws.BoolValue(bdm1.Ebs.DeleteOnTermination))

	// Second mapping - Empty EBS
	bdm2 := input.BlockDeviceMappings[1]
	assert.Equal(t, "/dev/sdh", aws.StringValue(bdm2.DeviceName))
	assert.NotNil(t, bdm2.Ebs)
	assert.Equal(t, int64(100), aws.Int64Value(bdm2.Ebs.VolumeSize))
	assert.Equal(t, "gp2", aws.StringValue(bdm2.Ebs.VolumeType))

	// Third mapping - Instance store
	bdm3 := input.BlockDeviceMappings[2]
	assert.Equal(t, "/dev/sdc", aws.StringValue(bdm3.DeviceName))
	assert.Equal(t, "ephemeral0", aws.StringValue(bdm3.VirtualName))
	assert.Nil(t, bdm3.Ebs)
}

func TestQueryParamsToStruct_CompleteRunInstancesExample(t *testing.T) {
	args := map[string]string{
		"Action":                              "RunInstances",
		"ImageId":                             "ami-0abcdef1234567890",
		"InstanceType":                        "t2.micro",
		"MinCount":                            "1",
		"MaxCount":                            "1",
		"SubnetId":                            "subnet-08fc749671b2d077c",
		"SecurityGroupId.1":                   "sg-0b0384b66d7d692f9",
		"KeyName":                             "MyKeyPair",
		"BlockDeviceMapping.1.DeviceName":     "/dev/sdh",
		"BlockDeviceMapping.1.Ebs.VolumeSize": "100",
		"TagSpecification.1.ResourceType":     "instance",
		"TagSpecification.1.Tag.1.Key":        "Name",
		"TagSpecification.1.Tag.1.Value":      "MyWebServer",
		"TagSpecification.1.Tag.2.Key":        "Environment",
		"TagSpecification.1.Tag.2.Value":      "Production",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)

	// Basic instance parameters
	assert.Equal(t, "ami-0abcdef1234567890", aws.StringValue(input.ImageId))
	assert.Equal(t, "t2.micro", aws.StringValue(input.InstanceType))
	assert.Equal(t, int64(1), aws.Int64Value(input.MinCount))
	assert.Equal(t, int64(1), aws.Int64Value(input.MaxCount))

	// Network parameters
	assert.Equal(t, "subnet-08fc749671b2d077c", aws.StringValue(input.SubnetId))
	assert.Len(t, input.SecurityGroupIds, 1)
	assert.Equal(t, "sg-0b0384b66d7d692f9", aws.StringValue(input.SecurityGroupIds[0]))
	assert.Equal(t, "MyKeyPair", aws.StringValue(input.KeyName))

	// Block device mappings
	assert.Len(t, input.BlockDeviceMappings, 1)
	bdm := input.BlockDeviceMappings[0]
	assert.Equal(t, "/dev/sdh", aws.StringValue(bdm.DeviceName))
	assert.NotNil(t, bdm.Ebs)
	assert.Equal(t, int64(100), aws.Int64Value(bdm.Ebs.VolumeSize))

	// Tag specifications
	assert.Len(t, input.TagSpecifications, 1)
	tagSpec := input.TagSpecifications[0]
	assert.Equal(t, "instance", aws.StringValue(tagSpec.ResourceType))
	assert.Len(t, tagSpec.Tags, 2)
	assert.Equal(t, "Name", aws.StringValue(tagSpec.Tags[0].Key))
	assert.Equal(t, "MyWebServer", aws.StringValue(tagSpec.Tags[0].Value))
	assert.Equal(t, "Environment", aws.StringValue(tagSpec.Tags[1].Key))
	assert.Equal(t, "Production", aws.StringValue(tagSpec.Tags[1].Value))
}

// --- Gap-filling tests for setFieldValue and setStructFields uncovered branches ---

func TestQueryParamsToStruct_ByteSliceBase64(t *testing.T) {
	// ImportKeyPairInput.PublicKeyMaterial is []byte — tests base64 decoding path
	args := map[string]string{
		"KeyName":           "my-imported-key",
		"PublicKeyMaterial": "c3NoLXJzYSB0ZXN0LWtleS1kYXRh", // base64 of "ssh-rsa test-key-data"
	}

	input := &ec2.ImportKeyPairInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, "my-imported-key", aws.StringValue(input.KeyName))
	assert.Equal(t, "ssh-rsa test-key-data", string(input.PublicKeyMaterial))
}

func TestQueryParamsToStruct_ByteSliceRawText(t *testing.T) {
	// Non-base64 text falls back to raw bytes
	args := map[string]string{
		"KeyName":           "raw-key",
		"PublicKeyMaterial": "not-valid-base64!!!",
	}

	input := &ec2.ImportKeyPairInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, []byte("not-valid-base64!!!"), input.PublicKeyMaterial)
}

func TestQueryParamsToStruct_InvalidIntValue(t *testing.T) {
	args := map[string]string{
		"MinCount": "not-a-number",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error setting field MinCount")
}

func TestQueryParamsToStruct_InvalidBoolValue(t *testing.T) {
	args := map[string]string{
		"InstanceId.1": "i-1234567890abcdef0",
		"Force":        "not-a-bool",
	}

	input := &ec2.StopInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "error setting field Force")
}

func TestQueryParamsToStruct_LocationNameCamelCaseFallback(t *testing.T) {
	// DeleteTagsInput.Resources has locationName:"resourceId"
	// AWS query params use "ResourceId" (titled), testing the camelCase → titleCase fallback
	args := map[string]string{
		"ResourceId.1": "i-1234567890abcdef0",
		"ResourceId.2": "i-0987654321fedcba0",
		"Tag.1.Key":    "Environment",
		"Tag.1.Value":  "Production",
	}

	input := &ec2.DeleteTagsInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.Resources, 2)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.Resources[0]))
	assert.Equal(t, "i-0987654321fedcba0", aws.StringValue(input.Resources[1]))
	assert.Len(t, input.Tags, 1)
	assert.Equal(t, "Environment", aws.StringValue(input.Tags[0].Key))
	assert.Equal(t, "Production", aws.StringValue(input.Tags[0].Value))
}

func TestQueryParamsToStruct_LocationNameDryRun(t *testing.T) {
	// DryRun has locationName:"dryRun" — tests that titled locationName matches "DryRun" query param
	args := map[string]string{
		"InstanceId.1": "i-1234567890abcdef0",
		"DryRun":       "true",
	}

	input := &ec2.TerminateInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, true, aws.BoolValue(input.DryRun))
}

func TestQueryParamsToStruct_NonStructPointer(t *testing.T) {
	args := map[string]string{"Key": "value"}
	str := "hello"
	err := QueryParamsToStruct(args, &str)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "must be a pointer to a struct")
}

func TestQueryParamsToStruct_BlockDeviceMappingsWithIOPS(t *testing.T) {
	args := map[string]string{
		"Action":                              "RunInstances",
		"ImageId":                             "ami-0abcdef1234567890",
		"InstanceType":                        "t2.micro",
		"MinCount":                            "1",
		"MaxCount":                            "1",
		"BlockDeviceMapping.1.DeviceName":     "/dev/sda1",
		"BlockDeviceMapping.1.Ebs.VolumeSize": "100",
		"BlockDeviceMapping.1.Ebs.VolumeType": "io2",
		"BlockDeviceMapping.1.Ebs.Iops":       "5000",
		"BlockDeviceMapping.1.Ebs.Throughput": "500",
		"BlockDeviceMapping.1.Ebs.KmsKeyId":   "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012",
	}

	input := &ec2.RunInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.BlockDeviceMappings, 1)

	bdm := input.BlockDeviceMappings[0]
	assert.Equal(t, "/dev/sda1", aws.StringValue(bdm.DeviceName))
	assert.NotNil(t, bdm.Ebs)
	assert.Equal(t, int64(100), aws.Int64Value(bdm.Ebs.VolumeSize))
	assert.Equal(t, "io2", aws.StringValue(bdm.Ebs.VolumeType))
	assert.Equal(t, int64(5000), aws.Int64Value(bdm.Ebs.Iops))
	assert.Equal(t, int64(500), aws.Int64Value(bdm.Ebs.Throughput))
	assert.Equal(t, "arn:aws:kms:us-east-1:123456789012:key/12345678-1234-1234-1234-123456789012", aws.StringValue(bdm.Ebs.KmsKeyId))
}

// --- ELBv2 member-style list tests ---

func TestQueryParamsToStruct_ELBv2RegisterTargets(t *testing.T) {
	// ELBv2 uses Targets.member.N.Id format (IAM-style query protocol)
	args := map[string]string{
		"Action":              "RegisterTargets",
		"TargetGroupArn":      "arn:aws:elasticloadbalancing:us-east-1:000000000001:targetgroup/my-tg/tg-abc123",
		"Targets.member.1.Id": "i-1234567890abcdef0",
		"Targets.member.2.Id": "i-0987654321fedcba0",
	}

	input := &elbv2.RegisterTargetsInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-east-1:000000000001:targetgroup/my-tg/tg-abc123", aws.StringValue(input.TargetGroupArn))
	assert.Len(t, input.Targets, 2)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.Targets[0].Id))
	assert.Equal(t, "i-0987654321fedcba0", aws.StringValue(input.Targets[1].Id))
}

func TestQueryParamsToStruct_ELBv2RegisterTargetsWithPort(t *testing.T) {
	args := map[string]string{
		"Action":                "RegisterTargets",
		"TargetGroupArn":        "arn:aws:elasticloadbalancing:us-east-1:000000000001:targetgroup/my-tg/tg-abc123",
		"Targets.member.1.Id":   "i-1234567890abcdef0",
		"Targets.member.1.Port": "8080",
	}

	input := &elbv2.RegisterTargetsInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.Targets, 1)
	assert.Equal(t, "i-1234567890abcdef0", aws.StringValue(input.Targets[0].Id))
	assert.Equal(t, int64(8080), aws.Int64Value(input.Targets[0].Port))
}

func TestQueryParamsToStruct_ELBv2CreateLoadBalancerMemberSubnets(t *testing.T) {
	// ELBv2 uses Subnets.member.N format for string slices
	args := map[string]string{
		"Action":           "CreateLoadBalancer",
		"Name":             "my-alb",
		"Subnets.member.1": "subnet-abc123",
		"Subnets.member.2": "subnet-def456",
	}

	input := &elbv2.CreateLoadBalancerInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, "my-alb", aws.StringValue(input.Name))
	assert.Len(t, input.Subnets, 2)
	assert.Equal(t, "subnet-abc123", aws.StringValue(input.Subnets[0]))
	assert.Equal(t, "subnet-def456", aws.StringValue(input.Subnets[1]))
}

func TestQueryParamsToStruct_ELBv2CreateListenerWithActions(t *testing.T) {
	args := map[string]string{
		"Action":                                 "CreateListener",
		"LoadBalancerArn":                        "arn:aws:elasticloadbalancing:us-east-1:000000000001:loadbalancer/app/my-alb/lb-abc123",
		"Protocol":                               "HTTP",
		"Port":                                   "80",
		"DefaultActions.member.1.Type":           "forward",
		"DefaultActions.member.1.TargetGroupArn": "arn:aws:elasticloadbalancing:us-east-1:000000000001:targetgroup/my-tg/tg-abc123",
	}

	input := &elbv2.CreateListenerInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Equal(t, "HTTP", aws.StringValue(input.Protocol))
	assert.Equal(t, int64(80), aws.Int64Value(input.Port))
	assert.Len(t, input.DefaultActions, 1)
	assert.Equal(t, "forward", aws.StringValue(input.DefaultActions[0].Type))
	assert.Equal(t, "arn:aws:elasticloadbalancing:us-east-1:000000000001:targetgroup/my-tg/tg-abc123", aws.StringValue(input.DefaultActions[0].TargetGroupArn))
}

func TestQueryParamsToStruct_StopsAtFirstGap(t *testing.T) {
	args := map[string]string{
		"Action":           "DescribeInstances",
		"Filter.1.Name":    "instance-type",
		"Filter.1.Value.1": "t2.micro",
		"Filter.3.Name":    "instance-state-name",
		"Filter.3.Value.1": "running",
	}

	input := &ec2.DescribeInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Len(t, input.Filters, 1)
	assert.Equal(t, "instance-type", aws.StringValue(input.Filters[0].Name))
}

func TestQueryParamsToStruct_GapAtIndexOne(t *testing.T) {
	args := map[string]string{
		"Action":           "DescribeInstances",
		"Filter.2.Name":    "skipped",
		"Filter.2.Value.1": "x",
	}

	input := &ec2.DescribeInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Empty(t, input.Filters)
}

func TestQueryParamsToStruct_RejectsOversizedDenseList(t *testing.T) {
	args := map[string]string{"Action": "DescribeInstances"}
	for i := 1; i <= maxSliceLen+1; i++ {
		args[fmt.Sprintf("Filter.%d.Name", i)] = "x"
		args[fmt.Sprintf("Filter.%d.Value.1", i)] = "y"
	}

	input := &ec2.DescribeInstancesInput{}
	err := QueryParamsToStruct(args, input)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum")
}

func TestQueryParamsToStruct_SparseHugeIndexNoOp(t *testing.T) {
	args := map[string]string{
		"Action":                "DescribeInstances",
		"Filter.999999.Name":    "ignored",
		"Filter.999999.Value.1": "y",
	}

	input := &ec2.DescribeInstancesInput{}
	err := QueryParamsToStruct(args, input)

	assert.NoError(t, err)
	assert.Empty(t, input.Filters)
}

func TestQueryParamsToStruct_CreateLaunchTemplateNestedData(t *testing.T) {
	args := map[string]string{
		"LaunchTemplateName":                                              "web-template",
		"LaunchTemplateData.ImageId":                                      "ami-0abcdef1234567890",
		"LaunchTemplateData.InstanceType":                                 "t3.micro",
		"LaunchTemplateData.BlockDeviceMapping.1.DeviceName":              "/dev/sda1",
		"LaunchTemplateData.BlockDeviceMapping.1.Ebs.VolumeSize":          "20",
		"LaunchTemplateData.InstanceMarketOptions.SpotOptions.ValidUntil": "2026-07-10T12:00:00Z",
	}

	input := &ec2.CreateLaunchTemplateInput{}
	err := QueryParamsToStruct(args, input)

	require.NoError(t, err)
	require.NotNil(t, input.LaunchTemplateData)
	assert.Equal(t, "ami-0abcdef1234567890", aws.StringValue(input.LaunchTemplateData.ImageId))
	require.Len(t, input.LaunchTemplateData.BlockDeviceMappings, 1)
	assert.Equal(t, int64(20), aws.Int64Value(input.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeSize))
	require.NotNil(t, input.LaunchTemplateData.InstanceMarketOptions)
	require.NotNil(t, input.LaunchTemplateData.InstanceMarketOptions.SpotOptions)
	assert.Equal(t, time.Date(2026, time.July, 10, 12, 0, 0, 0, time.UTC),
		*input.LaunchTemplateData.InstanceMarketOptions.SpotOptions.ValidUntil)
}
