package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/tags"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const clusterSGNamePrefix = "eks-cluster-sg-"

// sgCreatesIn returns the CreateSecurityGroup calls made against vpc whose name matches.
func sgCreatesIn(f *fakeSGProvisioner, vpc string, match func(name string) bool) []*ec2.CreateSecurityGroupInput {
	var out []*ec2.CreateSecurityGroupInput
	for _, c := range f.createCalls {
		if aws.StringValue(c.VpcId) == vpc && match(aws.StringValue(c.GroupName)) {
			out = append(out, c)
		}
	}
	return out
}

func sgCreateTags(c *ec2.CreateSecurityGroupInput) map[string]string {
	m := map[string]string{}
	for _, spec := range c.TagSpecifications {
		for k, v := range sgTagMap(spec.Tags) {
			m[k] = v
		}
	}
	return m
}

func authorizedOn(f *fakeSGProvisioner, sgID string) []*ec2.IpPermission {
	var out []*ec2.IpPermission
	for _, c := range f.authorizeCalls {
		if aws.StringValue(c.GroupId) == sgID {
			out = append(out, c.IpPermissions...)
		}
	}
	return out
}

func deletedSGIDs(f *fakeSGProvisioner) []string {
	var out []string
	for _, c := range f.deleteCalls {
		out = append(out, aws.StringValue(c.GroupId))
	}
	return out
}

// seedClusterSG registers an AWS-style cluster SG the way a created one looks.
func seedClusterSG(f *fakeSGProvisioner, cluster, vpc, id string) {
	f.existing[clusterSGNamePrefix+cluster+"-0123456789abcdef|"+vpc] = id
	if f.tagsByID == nil {
		f.tagsByID = map[string][]*ec2.Tag{}
	}
	f.tagsByID[id] = []*ec2.Tag{
		{Key: aws.String("kubernetes.io/cluster/" + cluster), Value: aws.String("owned")},
		{Key: aws.String("aws:eks:cluster-name"), Value: aws.String(cluster)},
		{Key: aws.String(clusterEKSClusterTagKey), Value: aws.String(cluster)},
	}
}

func createAlpha(t *testing.T, mutate func(*eks.CreateClusterInput)) (*eksServiceFixture, *ClusterMeta) {
	t.Helper()
	f := newEKSServiceFixture(t)
	f.sg.uniqueIDs = true
	in := createInput("alpha")
	if mutate != nil {
		mutate(in)
	}
	_, err := f.svc.CreateCluster(context.Background(), in, testAccountID, "")
	require.NoError(t, err)
	f.svc.WaitLaunches()
	meta, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	require.NoError(t, err)
	require.NotNil(t, meta.ResourcesVpcConfig)
	return f, meta
}

func withPrivateAccess(in *eks.CreateClusterInput) {
	in.ResourcesVpcConfig.EndpointPrivateAccess = aws.Bool(true)
}

// CreateCluster makes the one AWS-style cluster SG in the caller's VPC and
// reports it as clusterSecurityGroupId.
func TestCreateCluster_ClusterSGHasAWSNameTagsAndRules(t *testing.T) {
	t.Parallel()
	f, meta := createAlpha(t, nil)

	created := sgCreatesIn(f.sg, "vpc-aaa", func(n string) bool { return strings.HasPrefix(n, clusterSGNamePrefix+"alpha-") })
	require.Len(t, created, 1, "exactly one eks-cluster-sg-<name>-<id> SG in the customer VPC")
	name := aws.StringValue(created[0].GroupName)
	assert.NotEmpty(t, strings.TrimPrefix(name, clusterSGNamePrefix+"alpha-"), "name carries a unique id suffix")

	tg := sgCreateTags(created[0])
	assert.Equal(t, "owned", tg["kubernetes.io/cluster/alpha"])
	assert.Equal(t, "alpha", tg["aws:eks:cluster-name"])
	assert.NotEmpty(t, tg["Name"])
	assert.Equal(t, "alpha", tg[clusterEKSClusterTagKey])
	assert.Equal(t, tags.ManagedByEKS, tg[tags.ManagedByKey])

	id := f.sg.existing[name+"|vpc-aaa"]
	require.NotEmpty(t, id)
	assert.Equal(t, id, meta.ResourcesVpcConfig.ClusterSecurityGroupId)

	perms := authorizedOn(f.sg, id)
	require.Len(t, perms, 1, "inbound all from itself, nothing else")
	assert.Equal(t, "-1", aws.StringValue(perms[0].IpProtocol))
	require.Len(t, perms[0].UserIdGroupPairs, 1)
	assert.Equal(t, id, aws.StringValue(perms[0].UserIdGroupPairs[0].GroupId))
	for _, c := range f.sg.revokeEgressCall {
		assert.NotEqual(t, id, aws.StringValue(c.GroupId), "egress stays allow-all")
	}

	out, err := f.svc.DescribeCluster(context.Background(), &eks.DescribeClusterInput{Name: aws.String("alpha")}, testAccountID)
	require.NoError(t, err)
	assert.Equal(t, id, aws.StringValue(out.Cluster.ResourcesVpcConfig.ClusterSecurityGroupId))
}

// A new cluster has no customer-VPC control-plane or nodegroup SG, and exactly
// one SG per cluster carries kubernetes.io/cluster/<name>.
func TestCreateCluster_NoSeparateCustomerVPCControlPlaneOrNodegroupSG(t *testing.T) {
	t.Parallel()
	f, _ := createAlpha(t, withPrivateAccess)

	retired := sgCreatesIn(f.sg, "vpc-aaa", func(n string) bool {
		return n == ClusterControlPlaneSGName("alpha") || n == ClusterNodegroupSGName("alpha")
	})
	assert.Empty(t, retired, "customer VPC must not get control-plane-sg or nodegroup-sg")

	tagged := 0
	for _, c := range f.sg.createCalls {
		if _, ok := sgCreateTags(c)["kubernetes.io/cluster/alpha"]; ok {
			tagged++
		}
	}
	assert.Equal(t, 1, tagged, "exactly one SG tagged kubernetes.io/cluster/alpha")
}

// The private-endpoint ENI carries its own SG and the cluster SG.
func TestCreateCluster_PrivateEndpointENICarriesBothSGs(t *testing.T) {
	t.Parallel()
	f, meta := createAlpha(t, withPrivateAccess)

	var eni *ec2.CreateNetworkInterfaceInput
	for _, c := range f.vpc.createCalls {
		if strings.Contains(aws.StringValue(c.Description), "private-endpoint") {
			eni = c
		}
	}
	require.NotNil(t, eni, "private-endpoint ENI created")
	peSG := f.sg.existing[ClusterPrivateEndpointSGName("alpha")+"|vpc-aaa"]
	require.NotEmpty(t, peSG)
	assert.ElementsMatch(t,
		[]string{peSG, meta.ResourcesVpcConfig.ClusterSecurityGroupId},
		aws.StringValueSlice(eni.Groups))
	assert.NotContains(t, sgTagMap(f.sg.tagsByID[peSG]), "kubernetes.io/cluster/alpha",
		"only the cluster SG carries the cluster tag")
}

// The system control-plane VPC keeps its control-plane SG and rules.
func TestCreateCluster_SystemCPVPCSecurityGroupUnchanged(t *testing.T) {
	t.Parallel()
	f, meta := createAlpha(t, nil)
	require.NotNil(t, meta.ManagedCPVPC)
	cpVPC := meta.ManagedCPVPC.VpcId

	cp := sgCreatesIn(f.sg, cpVPC, func(n string) bool { return n == ClusterControlPlaneSGName("alpha") })
	require.Len(t, cp, 1)
	cpID := f.sg.existing[ClusterControlPlaneSGName("alpha")+"|"+cpVPC]
	require.NotEmpty(t, cpID)

	var ports []int64
	selfRef := false
	for _, p := range authorizedOn(f.sg, cpID) {
		ports = append(ports, aws.Int64Value(p.FromPort))
		for _, pair := range p.UserIdGroupPairs {
			if aws.StringValue(pair.GroupId) == cpID {
				selfRef = true
			}
		}
	}
	assert.Subset(t, ports, []int64{k3sAPIServerPort, konnectivityAgentPort, webhookReadyPort, 2379, 10250})
	assert.True(t, selfRef, "HA self-referencing rules stay")
	assert.Empty(t, sgCreatesIn(f.sg, cpVPC, func(n string) bool { return strings.HasPrefix(n, clusterSGNamePrefix) }),
		"the cluster SG is a customer-VPC resource only")
}

// Workers launch with exactly the cluster SG and no extra SG or cross rules.
func TestCreateNodegroup_WorkersGetExactlyTheClusterSG(t *testing.T) {
	t.Parallel()
	f := newEKSServiceFixture(t)
	f.sg.uniqueIDs = true
	seedActiveClusterWithToken(t, f, "c1")
	meta, err := GetClusterMeta(t.Context(), f.kv, "c1")
	require.NoError(t, err)
	meta.ResourcesVpcConfig.ClusterSecurityGroupId = "sg-cluster"
	require.NoError(t, PutClusterMeta(t.Context(), f.kv, meta))
	seedClusterSG(f.sg, "c1", "vpc-aaa", "sg-cluster")
	markWorkersReady(t, f, "c1", "ng1", 2)

	_, err = f.svc.CreateNodegroup(context.Background(), createNGInput("c1", "ng1", 2), testAccountID)
	require.NoError(t, err)
	f.svc.WaitLaunches()

	require.Len(t, f.worker.runCalls, 2)
	for _, c := range f.worker.runCalls {
		assert.Equal(t, []string{"sg-cluster"}, aws.StringValueSlice(c.SecurityGroupIds))
	}
	assert.Empty(t, f.sg.createCalls, "no nodegroup or control-plane SG is created for a worker")
	assert.Empty(t, f.sg.authorizeCalls, "no CP<->nodegroup cross rules")
}

// A launch template that names security groups gets exactly those.
func TestLaunchOneWorker_TemplateSecurityGroupsReplaceClusterSG(t *testing.T) {
	t.Parallel()
	reader := &fakeLaunchTemplates{out: &ec2.DescribeLaunchTemplateVersionsOutput{
		LaunchTemplateVersions: []*ec2.LaunchTemplateVersion{{
			LaunchTemplateData: &ec2.ResponseLaunchTemplateData{
				SecurityGroupIds: aws.StringSlice([]string{"sg-lt-1", "sg-lt-2"}),
			},
		}},
	}}
	f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-0123", Version: "1"}, reader)
	require.NoError(t, err)

	require.Len(t, f.worker.runCalls, 1)
	assert.Equal(t, []string{"sg-lt-1", "sg-lt-2"}, aws.StringValueSlice(f.worker.runCalls[0].SecurityGroupIds))
}

// A template naming no security groups falls back to the cluster SG alone.
func TestLaunchOneWorker_TemplateWithoutSecurityGroupsUsesClusterSG(t *testing.T) {
	t.Parallel()
	f, err := launchWorkerWith(t, &NodegroupLaunchTemplate{ID: "lt-0123", Version: "1"},
		&fakeLaunchTemplates{out: templateVersionWith(nil)})
	require.NoError(t, err)

	require.Len(t, f.worker.runCalls, 1)
	assert.Equal(t, []string{"sg-ng"}, aws.StringValueSlice(f.worker.runCalls[0].SecurityGroupIds))
}

// Delete finds the AWS-style cluster SG by tag, not by name, and leaves another
// cluster's SG in the same VPC alone.
func TestDeleteCluster_RemovesClusterSGFoundByTag(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	seedClusterSG(f.sg, "alpha", "vpc-aaa", "sg-new")
	seedClusterSG(f.sg, "beta", "vpc-aaa", "sg-beta")
	f.sg.perms = map[string][]*ec2.IpPermission{"sg-new": {{
		IpProtocol:       aws.String("-1"),
		UserIdGroupPairs: []*ec2.UserIdGroupPair{{GroupId: aws.String("sg-new")}},
	}}}
	f.sg.enforceDepViolation = true

	deleteAndSettle(t, f.svc, "alpha")

	deleted := deletedSGIDs(f.sg)
	assert.Contains(t, deleted, "sg-new")
	assert.NotContains(t, deleted, "sg-beta")
	_, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.Error(t, err, "teardown completed and removed the record")
}

// An old-layout cluster (control-plane-sg + nodegroup-sg, no cluster SG) still
// has both SGs removed.
func TestDeleteCluster_OldLayoutStillRemovesBothNamedSGs(t *testing.T) {
	t.Parallel()
	f := newDeleteClusterFixture(t, "alpha")
	f.sg.existing[ClusterControlPlaneSGName("alpha")+"|vpc-aaa"] = "sg-old-cp"
	f.sg.existing[ClusterNodegroupSGName("alpha")+"|vpc-aaa"] = "sg-old-ng"

	deleteAndSettle(t, f.svc, "alpha")

	deleted := deletedSGIDs(f.sg)
	assert.Contains(t, deleted, "sg-old-cp")
	assert.Contains(t, deleted, "sg-old-ng")
}

// A cluster SG that cannot be deleted yet keeps the cluster DELETING; the
// deleting reaper removes it once the dependency clears, not the request.
func TestDeleteCluster_ClusterSGConvergesInTheReaper(t *testing.T) {
	origBudget, origInterval := sgDeleteWaitBudget, sgDeleteWaitInterval
	sgDeleteWaitBudget, sgDeleteWaitInterval = 10*time.Millisecond, time.Millisecond
	t.Cleanup(func() { sgDeleteWaitBudget, sgDeleteWaitInterval = origBudget, origInterval })

	f := newDeleteClusterFixture(t, "alpha")
	seedClusterSG(f.sg, "alpha", "vpc-aaa", "sg-new")
	f.sg.deleteFailsBefore = map[string]int{"sg-new": 1000}

	deleteAndSettle(t, f.svc, "alpha")
	meta := requireTeardownRetained(t, f.kv, "alpha")
	assert.Contains(t, meta.DeleteFailedStep, stepSecurityGroups)

	f.sg.mu.Lock()
	f.sg.deleteFailsBefore["sg-new"] = 0
	f.sg.mu.Unlock()
	redrive(t, f.svc)

	_, err := GetClusterMeta(t.Context(), f.kv, "alpha")
	assert.Error(t, err, "the reaper finished the teardown")
}
