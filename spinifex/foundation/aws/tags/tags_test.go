package tags

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestIsSystemManaged(t *testing.T) {
	cases := map[string]struct {
		managedBy string
		want      bool
		why       string
	}{
		"elbv2": {ManagedByELBv2, true, "HAProxy VMs are platform-owned"},
		"eks":   {ManagedByEKS, true, "K3s control-plane VMs are platform-owned"},
		"rds":   {ManagedByRDS, true, "DB engine VMs are platform-owned and live in the system account"},
		// Container instances launched from the ECS node AMI stay customer-owned,
		// so only the AMI carries the tag — never a VM.
		"ecs":      {ManagedByECS, false, "ECS container instances are customer-owned"},
		"customer": {"", false, "an untagged instance is a customer instance"},
		"unknown":  {"redshift", false, "an unrecognised component is not system-managed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// A system VM that reports false never binds its
			// system.TerminateInstance.{id} subject, so a teardown invoked on a
			// non-owning node has no responder.
			assert.Equal(t, tc.want, IsSystemManaged(tc.managedBy), tc.why)
		})
	}
}

func TestExtract(t *testing.T) {
	specs := []*ec2.TagSpecification{
		{
			ResourceType: aws.String("instance"),
			Tags: []*ec2.Tag{
				{Key: aws.String("Name"), Value: aws.String("web-1")},
				{Key: nil, Value: aws.String("skipped-nil-key")},
				{Key: aws.String("skipped-nil-value"), Value: nil},
			},
		},
		{ResourceType: aws.String("volume"), Tags: []*ec2.Tag{{Key: aws.String("Env"), Value: aws.String("prod")}}},
	}

	assert.Equal(t, map[string]string{"Name": "web-1"}, Extract(specs, "instance"))
	empty := Extract(specs, "snapshot")
	assert.NotNil(t, empty)
	assert.Empty(t, empty)
}

func TestMapToEC2(t *testing.T) {
	got := MapToEC2(map[string]string{"Name": "web-1", "Env": "prod"})
	assert.Len(t, got, 2)
	asMap := make(map[string]string, len(got))
	for _, tag := range got {
		asMap[aws.StringValue(tag.Key)] = aws.StringValue(tag.Value)
	}
	assert.Equal(t, map[string]string{"Name": "web-1", "Env": "prod"}, asMap)
	assert.Nil(t, MapToEC2(nil))
	assert.Nil(t, MapToEC2(map[string]string{}))
}
