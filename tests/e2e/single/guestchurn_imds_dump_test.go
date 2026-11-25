//go:build e2e

package single

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/stretchr/testify/assert"
)

func TestGuestIMDSDumpWith(t *testing.T) {
	type call struct{ subnet, ip, dir string }
	record := func(calls *[]call) func(*testing.T, string, string, string) {
		return func(_ *testing.T, subnet, ip, dir string) {
			*calls = append(*calls, call{subnet, ip, dir})
		}
	}

	t.Run("the dump gets the instance's own subnet and private IP", func(t *testing.T) {
		var calls []call
		inst := &ec2.Instance{InstanceId: aws.String("i-1"), SubnetId: aws.String("subnet-a"), PrivateIpAddress: aws.String("172.31.0.4")}
		guestIMDSDumpWith(t, inst, "/art", record(&calls))()
		assert.Equal(t, []call{{"subnet-a", "172.31.0.4", "/art"}}, calls)
	})
	t.Run("an instance without a subnet or private IP dumps nothing", func(t *testing.T) {
		var calls []call
		guestIMDSDumpWith(t, &ec2.Instance{InstanceId: aws.String("i-2"), PrivateIpAddress: aws.String("172.31.0.4")}, "/art", record(&calls))()
		guestIMDSDumpWith(t, &ec2.Instance{InstanceId: aws.String("i-3"), SubnetId: aws.String("subnet-a")}, "/art", record(&calls))()
		assert.Empty(t, calls)
	})
}
