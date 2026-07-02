package daemon

import (
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	gateway_ec2_instance "github.com/mulgadc/spinifex/spinifex/gateway/ec2/instance"
	"github.com/nats-io/nats.go"
)

// eksCPControl is the NATS-backed control-plane recovery surface the EKS
// reconciler drives. DescribeInstances fans out across every host so a CP on
// any node is observed; RecoverInstance restarts the CP on its owning node —
// a live error/running owner in place via ec2.cmd.<id>, or a stopped instance
// rehydrated from the shared KV via ec2.start. Both reuse the same cluster-wide
// EC2 client helpers the gateway serves customer StartInstances/DescribeInstances
// requests with, so the reconciler recovers a CP wherever it landed.
type eksCPControl struct {
	natsConn      *nats.Conn
	expectedNodes func() int
}

// newEKSCPControl builds the reconciler's CP recovery surface bound to the
// daemon's NATS connection and cluster size.
func (d *Daemon) newEKSCPControl() *eksCPControl {
	return &eksCPControl{
		natsConn: d.natsConn,
		expectedNodes: func() int {
			if d.clusterConfig != nil {
				if n := len(d.clusterConfig.Nodes); n > 0 {
					return n
				}
			}
			return 1
		},
	}
}

// DescribeInstances fans out to every node and aggregates, so the reconciler
// sees the control-plane VM regardless of which host it runs on.
func (c *eksCPControl) DescribeInstances(input *ec2.DescribeInstancesInput, accountID string) (*ec2.DescribeInstancesOutput, error) {
	return gateway_ec2_instance.DescribeInstances(input, c.natsConn, c.expectedNodes(), accountID)
}

// RecoverInstance restarts the control-plane VM on its owning node. The gateway
// StartInstances two-step first targets a live owner (ec2.cmd.<id>) — recovering
// a crashed error-state CP in place, re-mounting its etcd root volume — then
// falls back to the shared-KV rehydration path (ec2.start) for a stopped CP.
func (c *eksCPControl) RecoverInstance(instanceID, accountID string) error {
	_, err := gateway_ec2_instance.StartInstances(&ec2.StartInstancesInput{
		InstanceIds: []*string{aws.String(instanceID)},
	}, c.natsConn, accountID)
	return err
}
