package daemon_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/daemon"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// benchVM approximates a real instance record. The env19 terminated bucket
// averages ~10KB per record, and Instance is nearly all of it.
func benchVM(id string) *vm.VM {
	now := time.Now()
	tags := make([]*ec2.Tag, 0, 12)
	for i := range 12 {
		tags = append(tags, &ec2.Tag{
			Key:   aws.String(fmt.Sprintf("tag-key-%d", i)),
			Value: aws.String(fmt.Sprintf("tag-value-%d-with-some-realistic-length", i)),
		})
	}
	enis := make([]*ec2.InstanceNetworkInterface, 0, 2)
	for i := range 2 {
		enis = append(enis, &ec2.InstanceNetworkInterface{
			NetworkInterfaceId: aws.String(fmt.Sprintf("eni-%s-%d", id, i)),
			MacAddress:         aws.String("02:00:00:00:00:01"),
			PrivateIpAddress:   aws.String("172.31.0.5"),
			SubnetId:           aws.String("subnet-0123456789abcdef0"),
			VpcId:              aws.String("vpc-0123456789abcdef0"),
			Status:             aws.String("in-use"),
			Groups: []*ec2.GroupIdentifier{
				{GroupId: aws.String("sg-0123456789abcdef0"), GroupName: aws.String("default")},
			},
		})
	}
	return &vm.VM{
		ID:           id,
		AccountID:    "111122223333",
		InstanceType: "t3.micro",
		Status:       vm.StateRunning,
		LastNode:     "node-1",
		DesiredState: vm.DesiredRunning,
		Instance: &ec2.Instance{
			InstanceId:         aws.String(id),
			ImageId:            aws.String("ami-0123456789abcdef0"),
			InstanceType:       aws.String("t3.micro"),
			KeyName:            aws.String("my-key"),
			LaunchTime:         &now,
			PrivateIpAddress:   aws.String("172.31.0.5"),
			PrivateDnsName:     aws.String("ip-172-31-0-5.ap-southeast-2.compute.internal"),
			SubnetId:           aws.String("subnet-0123456789abcdef0"),
			VpcId:              aws.String("vpc-0123456789abcdef0"),
			Architecture:       aws.String("x86_64"),
			RootDeviceName:     aws.String("/dev/sda1"),
			RootDeviceType:     aws.String("ebs"),
			VirtualizationType: aws.String("hvm"),
			Hypervisor:         aws.String("xen"),
			State:              &ec2.InstanceState{Code: aws.Int64(16), Name: aws.String("running")},
			Placement:          &ec2.Placement{AvailabilityZone: aws.String("ap-southeast-2a")},
			Monitoring:         &ec2.Monitoring{State: aws.String("disabled")},
			MetadataOptions: &ec2.InstanceMetadataOptionsResponse{
				HttpEndpoint: aws.String("enabled"), HttpTokens: aws.String("required"),
			},
			NetworkInterfaces: enis,
			Tags:              tags,
		},
	}
}

func benchSet(n int) map[string]*vm.VM {
	set := make(map[string]*vm.VM, n)
	for i := range n {
		id := fmt.Sprintf("i-%017d", i)
		set[id] = benchVM(id)
	}
	return set
}

// What one instance's state change costs while the blob is what gets written:
// the whole node's set, marshalled every time. Reports the blob size alongside
// the time so the amplification is readable from the benchmark output.
func BenchmarkMarshalBlob(b *testing.B) {
	for _, n := range []int{1, 10, 50, 100} {
		set := benchSet(n)
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			var blob []byte
			for b.Loop() {
				var err error
				if blob, err = daemon.MarshalLocalState(set); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(len(blob)), "blob_bytes")
		})
	}
}

// The counterpart: what the same state change costs once the record is what
// gets written. Constant in N, which is the whole point of the split.
func BenchmarkMarshalOneRecord(b *testing.B) {
	record := benchVM("i-0").Record()
	b.ReportAllocs()
	var blob []byte
	for b.Loop() {
		var err error
		if blob, err = json.Marshal(record); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(blob)), "blob_bytes")
}
