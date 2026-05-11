package handlers_ec2_instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/filterutil"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstanceMatchesFilters_NoFilters(t *testing.T) {
	inst := &vm.VM{ID: "i-123", InstanceType: "t3.micro"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-123"),
		InstanceType: aws.String("t3.micro"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	assert.True(t, instanceMatchesFilters(inst, ic, nil))
}

func TestInstanceMatchesFilters_SingleFilter_Match(t *testing.T) {
	inst := &vm.VM{ID: "i-abc", InstanceType: "t3.micro"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-abc"),
		InstanceType: aws.String("t3.micro"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{
		"instance-state-name": {"running"},
	}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_SingleFilter_NoMatch(t *testing.T) {
	inst := &vm.VM{ID: "i-abc", InstanceType: "t3.micro"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-abc"),
		InstanceType: aws.String("t3.micro"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{
		"instance-state-name": {"stopped"},
	}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_MultipleValues_ORLogic(t *testing.T) {
	inst := &vm.VM{ID: "i-abc", InstanceType: "t3.micro"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-abc"),
		InstanceType: aws.String("t3.micro"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{
		"instance-state-name": {"running", "stopped"},
	}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_MultipleFilters_ANDLogic(t *testing.T) {
	inst := &vm.VM{ID: "i-abc", InstanceType: "t3.micro"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-abc"),
		InstanceType: aws.String("t3.micro"),
		VpcId:        aws.String("vpc-111"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{
		"instance-state-name": {"running"},
		"vpc-id":              {"vpc-111"},
	}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))

	filters["vpc-id"] = []string{"vpc-999"}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_InstanceId(t *testing.T) {
	inst := &vm.VM{ID: "i-abc"}
	ic := &ec2.Instance{
		InstanceId: aws.String("i-abc"),
		State:      &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{"instance-id": {"i-abc"}}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))

	filters["instance-id"] = []string{"i-other"}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_InstanceType(t *testing.T) {
	inst := &vm.VM{ID: "i-abc", InstanceType: "m5.large"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-abc"),
		InstanceType: aws.String("m5.large"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{"instance-type": {"m5.large"}}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_SubnetId(t *testing.T) {
	inst := &vm.VM{ID: "i-abc"}
	ic := &ec2.Instance{
		InstanceId: aws.String("i-abc"),
		SubnetId:   aws.String("subnet-123"),
		State:      &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{"subnet-id": {"subnet-123"}}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))

	filters["subnet-id"] = []string{"subnet-999"}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_Wildcard(t *testing.T) {
	inst := &vm.VM{ID: "i-abc", InstanceType: "t3.micro"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-abc"),
		InstanceType: aws.String("t3.micro"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{"instance-type": {"t3.*"}}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))

	filters["instance-type"] = []string{"m5.*"}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_TagFilter(t *testing.T) {
	inst := &vm.VM{ID: "i-abc"}
	ic := &ec2.Instance{
		InstanceId: aws.String("i-abc"),
		State:      &ec2.InstanceState{Name: aws.String("running")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("prod")},
			{Key: aws.String("Team"), Value: aws.String("backend")},
		},
	}

	filters := map[string][]string{"tag:Environment": {"prod"}}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))

	filters["tag:Environment"] = []string{"staging"}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_TagKey(t *testing.T) {
	inst := &vm.VM{ID: "i-abc"}
	ic := &ec2.Instance{
		InstanceId: aws.String("i-abc"),
		State:      &ec2.InstanceState{Name: aws.String("running")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("prod")},
		},
	}
	filters := map[string][]string{"tag-key": {"Environment"}}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))

	filters["tag-key"] = []string{"MissingKey"}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_TagValue(t *testing.T) {
	inst := &vm.VM{ID: "i-abc"}
	ic := &ec2.Instance{
		InstanceId: aws.String("i-abc"),
		State:      &ec2.InstanceState{Name: aws.String("running")},
		Tags: []*ec2.Tag{
			{Key: aws.String("Environment"), Value: aws.String("prod")},
		},
	}
	filters := map[string][]string{"tag-value": {"prod"}}
	assert.True(t, instanceMatchesFilters(inst, ic, filters))

	filters["tag-value"] = []string{"staging"}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestInstanceMatchesFilters_EmptyResults(t *testing.T) {
	inst := &vm.VM{ID: "i-abc", InstanceType: "t3.micro"}
	ic := &ec2.Instance{
		InstanceId:   aws.String("i-abc"),
		InstanceType: aws.String("t3.micro"),
		State:        &ec2.InstanceState{Name: aws.String("running")},
	}
	filters := map[string][]string{"instance-type": {"c5.4xlarge"}}
	assert.False(t, instanceMatchesFilters(inst, ic, filters))
}

func TestParseFilters_UnknownFilterName(t *testing.T) {
	filters := []*ec2.Filter{
		{Name: aws.String("bogus-filter"), Values: []*string{aws.String("val")}},
	}
	_, err := filterutil.ParseFilters(filters, DescribeInstancesValidFilters)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "InvalidParameterValue")
}

func TestEC2TagsToMap(t *testing.T) {
	tags := []*ec2.Tag{
		{Key: aws.String("a"), Value: aws.String("1")},
		{Key: aws.String("b"), Value: aws.String("2")},
	}
	m := filterutil.EC2TagsToMap(tags)
	assert.Equal(t, "1", m["a"])
	assert.Equal(t, "2", m["b"])
}

func TestEC2TagsToMap_Nil(t *testing.T) {
	assert.Nil(t, filterutil.EC2TagsToMap(nil))
}
