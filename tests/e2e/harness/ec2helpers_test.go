//go:build e2e

package harness

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
)

type fakeImagesEC2 struct {
	ec2iface.EC2API
	images []*ec2.Image
}

func (f *fakeImagesEC2) DescribeImages(*ec2.DescribeImagesInput) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: f.images}, nil
}

func TestPreferredAMI_FallbackOrder(t *testing.T) {
	img := func(id, name string) *ec2.Image {
		return &ec2.Image{ImageId: aws.String(id), Name: aws.String(name)}
	}
	cases := []struct {
		name   string
		images []*ec2.Image
		want   string
	}{
		{"ubuntu beats earlier non-alpine", []*ec2.Image{
			img("ami-alp", "ami-alpine-3.21-x86_64"),
			img("ami-deb", "ami-debian-12-x86_64"),
			img("ami-ubu", "ami-ubuntu-24.04-x86_64"),
		}, "ami-ubu"},
		{"non-alpine beats earlier alpine", []*ec2.Image{
			img("ami-alp", "ami-Alpine-3.21-x86_64"),
			img("ami-deb", "ami-debian-12-x86_64"),
		}, "ami-deb"},
		{"alpine when nothing else", []*ec2.Image{
			img("ami-alp", "ami-alpine-3.21-x86_64"),
		}, "ami-alp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &AWSClient{EC2: &fakeImagesEC2{images: tc.images}}
			if got := PreferredAMI(t, c); got != tc.want {
				t.Fatalf("PreferredAMI = %q, want %q", got, tc.want)
			}
		})
	}
}
