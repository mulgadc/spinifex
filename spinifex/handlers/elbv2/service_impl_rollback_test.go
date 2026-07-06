package handlers_elbv2

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/elbv2"
	handlers_ec2_vpc "github.com/mulgadc/spinifex/spinifex/handlers/ec2/vpc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managedENIIDs returns every requester-managed ENI in the account.
func managedENIIDs(t *testing.T, vpcSvc *handlers_ec2_vpc.VPCServiceImpl) []string {
	t.Helper()
	out, err := vpcSvc.DescribeNetworkInterfaces(context.Background(), &ec2.DescribeNetworkInterfacesInput{}, testAccountID)
	require.NoError(t, err)
	var ids []string
	for _, eni := range out.NetworkInterfaces {
		if eni.RequesterManaged != nil && *eni.RequesterManaged {
			ids = append(ids, *eni.NetworkInterfaceId)
		}
	}
	return ids
}

// TestRollbackLBInfra_RemovesAllInfra verifies the rollback the
// PutLoadBalancer-failure path runs leaves no orphan: ENIs deleted, managed SG
// deleted, name claim released (so the name is immediately reclaimable).
func TestRollbackLBInfra_RemovesAllInfra(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-rollback", "internal", subnetID)
	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	sgID := rec.NLBManagedSGID
	require.NotEmpty(t, sgID)

	eniIDs := managedENIIDs(t, vpcSvc)
	require.NotEmpty(t, eniIDs, "NLB create must mint at least one managed ENI")

	// Roll back exactly as the PutLoadBalancer-failure path does.
	svc.rollbackLBInfra(context.Background(), eniIDs, sgID, "nlb-rollback", testAccountID)

	// ENIs gone.
	for _, id := range managedENIIDs(t, vpcSvc) {
		assert.NotContains(t, eniIDs, id, "rolled-back ENI must be deleted")
	}

	// Managed SG gone (empty result or not-found are both acceptable).
	sgOut, err := vpcSvc.DescribeSecurityGroups(context.Background(), &ec2.DescribeSecurityGroupsInput{
		GroupIds: aws.StringSlice([]string{sgID}),
	}, testAccountID)
	if err == nil {
		assert.Empty(t, sgOut.SecurityGroups, "rolled-back managed SG must be deleted")
	}

	// Name claim released → reclaimable by a fresh owner.
	ok, dup, err := svc.store.ClaimLBName("nlb-rollback", testAccountID, "lb-fresh")
	require.NoError(t, err)
	assert.True(t, ok, "rollback must release the name claim")
	assert.False(t, dup)
}

// TestRollbackListener_RevokesPortAndDeletesRecord covers the
// updateStoredConfig-failure path: the listener port was already authorized, so
// rollback must revoke it AND remove the persisted listener record.
func TestRollbackListener_RevokesPortAndDeletesRecord(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-lst-rb", "internet-facing", subnetID)
	createTCPListener(t, svc, lb.LoadBalancerArn, 443)

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)
	require.True(t, sgHasRule(describeSG(t, vpcSvc, rec.NLBManagedSGID), "tcp", 443, "0.0.0.0/0"))

	lst, err := svc.DescribeListeners(context.Background(), &elbv2.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn}, testAccountID)
	require.NoError(t, err)
	require.Len(t, lst.Listeners, 1)
	listenerRec, err := svc.store.GetListenerByArn(*lst.Listeners[0].ListenerArn)
	require.NoError(t, err)

	// authorizedCIDRs non-empty ⇒ the config-failure path that already opened the port.
	svc.rollbackListener(context.Background(), listenerRec, rec, "tcp", 443, []string{"0.0.0.0/0"}, testAccountID)

	lst, err = svc.DescribeListeners(context.Background(), &elbv2.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, lst.Listeners, "rolled-back listener record must be deleted")

	assert.False(t, sgHasRule(describeSG(t, vpcSvc, rec.NLBManagedSGID), "tcp", 443, "0.0.0.0/0"),
		"config-failure rollback must revoke the authorized port")
}

// TestRollbackListener_NilCIDRsSkipsRevoke covers the authorize-failure path:
// the port was never successfully opened, so rollback deletes the listener
// record but must not touch SG authorizations (authorizedCIDRs is nil).
func TestRollbackListener_NilCIDRsSkipsRevoke(t *testing.T) {
	svc, vpcSvc := setupTestServiceWithVPC(t)
	subnetID, _ := firstSubnet(t, vpcSvc)

	lb := createNLB(t, svc, "nlb-lst-nil", "internet-facing", subnetID)
	createTCPListener(t, svc, lb.LoadBalancerArn, 443)

	rec, err := svc.store.GetLoadBalancerByArn(*lb.LoadBalancerArn)
	require.NoError(t, err)

	lst, err := svc.DescribeListeners(context.Background(), &elbv2.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn}, testAccountID)
	require.NoError(t, err)
	require.Len(t, lst.Listeners, 1)
	listenerRec, err := svc.store.GetListenerByArn(*lst.Listeners[0].ListenerArn)
	require.NoError(t, err)

	svc.rollbackListener(context.Background(), listenerRec, rec, "tcp", 443, nil, testAccountID)

	lst, err = svc.DescribeListeners(context.Background(), &elbv2.DescribeListenersInput{LoadBalancerArn: lb.LoadBalancerArn}, testAccountID)
	require.NoError(t, err)
	assert.Empty(t, lst.Listeners, "rolled-back listener record must be deleted")

	assert.True(t, sgHasRule(describeSG(t, vpcSvc, rec.NLBManagedSGID), "tcp", 443, "0.0.0.0/0"),
		"nil authorizedCIDRs must skip the SG revoke")
}
