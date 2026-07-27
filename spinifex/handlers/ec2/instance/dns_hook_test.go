package handlers_ec2_instance

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/vm"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// publishDNS must be a safe no-op when northstar is not configured (no base
// domain) and must tolerate a nil NATS connection while building changes.
func TestPublishDNS(t *testing.T) {
	inst := &vm.VM{
		PublicIP: "1.2.3.4",
		Instance: &ec2.Instance{PrivateIpAddress: aws.String("172.31.26.216")},
	}

	// Disabled: no base domain → no panic, no publish attempt.
	disabled := &InstanceServiceImpl{config: &config.Config{Region: "ap-southeast-2"}}
	disabled.publishDNS("123456789012", handlers_dns.ActionUpsert, []*vm.VM{inst})

	// Enabled base domain but nil NATS conn: builds changes, best-effort publish
	// is a no-op for a nil connection (must not panic).
	enabled := &InstanceServiceImpl{
		config:        &config.Config{Region: "ap-southeast-2"},
		dnsBaseDomain: "spx3.net",
	}
	enabled.publishDNS("123456789012", handlers_dns.ActionUpsert, []*vm.VM{inst})
	enabled.publishDNS("123456789012", handlers_dns.ActionDelete, []*vm.VM{inst})
	enabled.publishDNS("123456789012", handlers_dns.ActionUpsert, nil)
}

func TestNeedsDNSWithdrawal(t *testing.T) {
	tests := []struct {
		name   string
		status vm.InstanceState
		want   bool
	}{
		{name: "running", status: vm.StateRunning, want: false},
		{name: "stopping", status: vm.StateStopping, want: false},
		{name: "stopped", status: vm.StateStopped, want: false},
		{name: "shutting down", status: vm.StateShuttingDown, want: true},
		{name: "terminated", status: vm.StateTerminated, want: true},
		{name: "error", status: vm.StateError, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, needsDNSWithdrawal(tt.status))
		})
	}
}

func TestPrepareRunInstancesDoesNotPublishDNS(t *testing.T) {
	eni := &fakeENICreator{
		defaultSubnet: &SubnetInfo{SubnetID: "subnet-default", VpcID: "vpc-1"},
		createOut: &ec2.CreateNetworkInterfaceOutput{NetworkInterface: &ec2.NetworkInterface{
			NetworkInterfaceId: aws.String("eni-1"),
			MacAddress:         aws.String("aa:bb:cc:dd:ee:ff"),
			PrivateIpAddress:   aws.String("10.0.0.10"),
			VpcId:              aws.String("vpc-1"),
		}},
	}
	svc, _ := prepareSvcWithENI(t, eni, nil)
	svc.dnsBaseDomain = "spx3.net"

	published := make(chan struct{}, 1)
	sub, err := svc.natsConn.Subscribe(handlers_dns.SubjectRecordsetChange, func(msg *nats.Msg) {
		published <- struct{}{}
		_ = msg.Respond([]byte(`{"applied":1}`))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	require.NoError(t, svc.natsConn.Flush())

	_, _, _, err = svc.PrepareRunInstances(context.Background(), &ec2.RunInstancesInput{
		InstanceType: aws.String("t3.micro"),
		ImageId:      aws.String("ami-1"),
		MinCount:     aws.Int64(1),
		MaxCount:     aws.Int64(1),
	}, "acc", "")
	require.NoError(t, err)

	select {
	case <-published:
		t.Fatal("PrepareRunInstances must not publish DNS before VM launch succeeds")
	case <-time.After(100 * time.Millisecond):
	}
}
