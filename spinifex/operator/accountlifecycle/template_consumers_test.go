package accountlifecycle

//test:in-package — shares the scripted EKS responder with the reaper tests.

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/eks"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func nodegroupNaming(id, name string) eks.DescribeNodegroupOutput {
	return eks.DescribeNodegroupOutput{Nodegroup: &eks.Nodegroup{
		LaunchTemplate: &eks.LaunchTemplateSpecification{Id: aws.String(id), Name: aws.String(name)},
	}}
}

// Every nodegroup of every cluster is described, whatever its status, and the
// template IDs and names they carry are collected.
func TestTemplateConsumersCollectEveryNodegroupsTemplate(t *testing.T) {
	fake := newFakeEKS(t)
	fake.reply("eks.ListClusters", eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"alpha"})})
	fake.reply("eks.ListNodegroups", eks.ListNodegroupsOutput{Nodegroups: aws.StringSlice([]string{"ng1", "ng2", "ng3"})})
	fake.reply("eks.DescribeNodegroup",
		nodegroupNaming("lt-1", ""),
		nodegroupNaming("", "by-name"),
		eks.DescribeNodegroupOutput{Nodegroup: &eks.Nodegroup{Status: aws.String(eks.NodegroupStatusCreateFailed)}},
	)

	got, err := NewTemplateConsumerReader(fake.nc).AccountTemplateConsumers(testCtx(t), "000000000042")

	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"lt-1": true}, got.TemplateIDs)
	assert.Equal(t, map[string]bool{"by-name": true}, got.TemplateNames)
	assert.Equal(t, 3, fake.requestCount("eks.DescribeNodegroup"))
}

// A listing or describe that fails is an error, never a short answer: a
// template the unread nodegroup names would otherwise look unreferenced.
func TestTemplateConsumersFailClosed(t *testing.T) {
	cases := map[string]func(*fakeEKS){
		"cluster listing fails": func(f *fakeEKS) { f.fail("eks.ListClusters", awserrors.ErrorServerInternal) },
		"nodegroup listing fails": func(f *fakeEKS) {
			f.reply("eks.ListClusters", eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"alpha"})})
			f.fail("eks.ListNodegroups", awserrors.ErrorServerInternal)
		},
		"describe fails": func(f *fakeEKS) {
			f.reply("eks.ListClusters", eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"alpha"})})
			f.reply("eks.ListNodegroups", eks.ListNodegroupsOutput{Nodegroups: aws.StringSlice([]string{"ng1", "ng2"})})
			f.reply("eks.DescribeNodegroup", nodegroupNaming("lt-1", ""))
			f.fail("eks.DescribeNodegroup", awserrors.ErrorServerInternal)
		},
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeEKS(t)
			script(fake)

			got, err := NewTemplateConsumerReader(fake.nc).AccountTemplateConsumers(testCtx(t), "000000000042")

			require.Error(t, err)
			assert.Nil(t, got)
		})
	}
}

// A cluster or nodegroup that vanishes between the calls is gone, and holds no
// reference.
func TestTemplateConsumersSkipWhatWentBetweenCalls(t *testing.T) {
	fake := newFakeEKS(t)
	fake.reply("eks.ListClusters", eks.ListClustersOutput{Clusters: aws.StringSlice([]string{"alpha"})})
	fake.fail("eks.ListNodegroups", awserrors.ErrorEKSResourceNotFound)

	got, err := NewTemplateConsumerReader(fake.nc).AccountTemplateConsumers(testCtx(t), "000000000042")

	require.NoError(t, err)
	assert.Empty(t, got.TemplateIDs)
	assert.Empty(t, got.TemplateNames)
}
