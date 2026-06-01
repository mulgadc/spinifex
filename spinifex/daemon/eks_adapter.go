package daemon

import (
	"errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	handlers_eks "github.com/mulgadc/spinifex/spinifex/handlers/eks"
	spxtypes "github.com/mulgadc/spinifex/spinifex/types"
)

// daemonEKSSubnetResolver adapts the daemon's VPCServiceImpl to the
// handlers_eks subnetVPCResolver-equivalent. handlers/eks declares the
// interface privately; the daemon needs an exported shape only because the
// constructor wires it via EKSServiceDeps.SubnetResolver = ...
//
// The translation is trivial: VPCService.GetSubnet returns a SubnetRecord
// with VpcId, which is the only field the EKS launcher needs.
type daemonEKSSubnetResolver struct {
	d *Daemon
}

func (a *daemonEKSSubnetResolver) GetSubnetVPC(accountID, subnetID string) (string, error) {
	if a.d == nil || a.d.vpcService == nil {
		return "", errors.New("eks: VPC service not initialized")
	}
	rec, err := a.d.vpcService.GetSubnet(accountID, subnetID)
	if err != nil {
		return "", err
	}
	return rec.VpcId, nil
}

var _ handlers_eks.SubnetVPCResolver = (*daemonEKSSubnetResolver)(nil)

// daemonEKSInstanceLauncher adapts the daemon's InstanceServiceImpl + vmMgr
// onto the launcher contract handlers/eks expects. RunInstances delegates
// directly; TerminateInstances looks each ID up in the local vm.Manager and
// calls StopOrTerminateInstance (the same path the per-instance ec2.cmd
// NATS subject takes), staying in-process so DeleteCluster can run as one
// transaction.
type daemonEKSInstanceLauncher struct {
	d *Daemon
}

func (a *daemonEKSInstanceLauncher) RunInstances(input *ec2.RunInstancesInput, accountID string) (*ec2.Reservation, error) {
	if a.d == nil || a.d.instanceService == nil {
		return nil, errors.New("eks: instance service not initialized")
	}
	return a.d.instanceService.RunInstances(input, accountID)
}

func (a *daemonEKSInstanceLauncher) TerminateInstances(input *ec2.TerminateInstancesInput, _ string) (*ec2.TerminateInstancesOutput, error) {
	if a.d == nil || a.d.instanceService == nil || a.d.vmMgr == nil {
		return nil, errors.New("eks: instance service not initialized")
	}
	out := &ec2.TerminateInstancesOutput{}
	for _, idPtr := range input.InstanceIds {
		if idPtr == nil || *idPtr == "" {
			continue
		}
		instance, ok := a.d.vmMgr.Get(*idPtr)
		if !ok {
			continue
		}
		cmd := spxtypes.EC2InstanceCommand{
			ID:         *idPtr,
			Attributes: spxtypes.EC2CommandAttributes{TerminateInstance: true},
		}
		if err := a.d.instanceService.StopOrTerminateInstance(instance, cmd); err != nil {
			return nil, err
		}
		out.TerminatingInstances = append(out.TerminatingInstances, &ec2.InstanceStateChange{
			InstanceId: aws.String(*idPtr),
			CurrentState: &ec2.InstanceState{
				Name: aws.String("shutting-down"),
			},
		})
	}
	return out, nil
}
