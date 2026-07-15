package handlers_ec2_image

import (
	"context"

	"github.com/aws/aws-sdk-go/service/ec2"
)

// ImageService defines the interface for EC2 image operations business logic.
type ImageService interface {
	CreateImage(ctx context.Context, input *ec2.CreateImageInput, accountID string) (*ec2.CreateImageOutput, error)
	CopyImage(ctx context.Context, input *ec2.CopyImageInput, accountID string) (*ec2.CopyImageOutput, error)
	DescribeImages(ctx context.Context, input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error)
	DescribeImageAttribute(ctx context.Context, input *ec2.DescribeImageAttributeInput, accountID string) (*ec2.DescribeImageAttributeOutput, error)
	RegisterImage(ctx context.Context, input *ec2.RegisterImageInput, accountID string) (*ec2.RegisterImageOutput, error)
	DeregisterImage(ctx context.Context, input *ec2.DeregisterImageInput, accountID string) (*ec2.DeregisterImageOutput, error)
	ModifyImageAttribute(ctx context.Context, input *ec2.ModifyImageAttributeInput, accountID string) (*ec2.ModifyImageAttributeOutput, error)
	ResetImageAttribute(ctx context.Context, input *ec2.ResetImageAttributeInput, accountID string) (*ec2.ResetImageAttributeOutput, error)
}
