package handlers_systemvpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/tags"
)

// IGWProvisioner is the narrow Internet Gateway surface the system VPC needs.
// The external switch, localnet and gateway LRP that make a front-end IP
// answerable on the wire are built only once an IGW is attached, and a system
// VPC has no customer to provision one.
type IGWProvisioner interface {
	DescribeInternetGateways(ctx context.Context, input *ec2.DescribeInternetGatewaysInput, accountID string) (*ec2.DescribeInternetGatewaysOutput, error)
	CreateInternetGateway(ctx context.Context, input *ec2.CreateInternetGatewayInput, accountID string) (*ec2.CreateInternetGatewayOutput, error)
	AttachInternetGateway(ctx context.Context, input *ec2.AttachInternetGatewayInput, accountID string) (*ec2.AttachInternetGatewayOutput, error)
	DetachInternetGateway(ctx context.Context, input *ec2.DetachInternetGatewayInput, accountID string) (*ec2.DetachInternetGatewayOutput, error)
	DeleteInternetGateway(ctx context.Context, input *ec2.DeleteInternetGatewayInput, accountID string) (*ec2.DeleteInternetGatewayOutput, error)
}

// EnsureIGW guarantees vpcID has an attached Internet Gateway. An IGW already
// attached is reused as-is and left untagged; only when none exists is an
// owner-tagged one created.
//
// Exported so a component needing an IGW on a customer VPC reaches it through
// here rather than duplicating the reuse rule.
func EnsureIGW(ctx context.Context, igwp IGWProvisioner, owner Owner, accountID, vpcID string) error {
	if vpcID == "" {
		return errors.New("systemvpc: EnsureIGW empty vpc id")
	}
	if owner.Name == "" {
		return errors.New("systemvpc: EnsureIGW empty owner name")
	}

	existing, err := AttachedIGW(ctx, igwp, accountID, vpcID)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	out, err := igwp.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{
		TagSpecifications: []*ec2.TagSpecification{{
			ResourceType: aws.String("internet-gateway"),
			Tags: []*ec2.Tag{
				{Key: aws.String(tags.ManagedByKey), Value: aws.String(owner.ManagedBy)},
				{Key: aws.String(owner.OwnerTagKey), Value: aws.String(owner.Name)},
			},
		}},
	}, accountID)
	if err != nil {
		return fmt.Errorf("systemvpc: create IGW for vpc %s: %w", vpcID, err)
	}
	if out == nil || out.InternetGateway == nil || aws.StringValue(out.InternetGateway.InternetGatewayId) == "" {
		return fmt.Errorf("systemvpc: create IGW for vpc %s: empty gateway id", vpcID)
	}
	igwID := aws.StringValue(out.InternetGateway.InternetGatewayId)

	if _, err := igwp.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}, accountID); err != nil {
		return fmt.Errorf("systemvpc: attach IGW %s to vpc %s: %w", igwID, vpcID, err)
	}
	slog.InfoContext(ctx, "systemvpc: attached IGW", "igw", igwID, "vpc", vpcID, "owner", owner.Name)
	return nil
}

// DeleteIGW detaches and deletes the owner's IGW attached to vpcID. Best-effort
// and ownership-scoped: it only removes an IGW carrying this owner's tags, so a
// customer-provisioned IGW that EnsureIGW reused is never deleted.
func DeleteIGW(ctx context.Context, igwp IGWProvisioner, owner Owner, accountID, vpcID string) error {
	if vpcID == "" || owner.Name == "" {
		return errors.New("systemvpc: DeleteIGW empty vpc id or owner name")
	}

	igw, err := AttachedIGW(ctx, igwp, accountID, vpcID)
	if err != nil {
		slog.WarnContext(ctx, "systemvpc DeleteIGW: IGW lookup failed", "vpc", vpcID, "err", err)
		return nil
	}
	if igw == nil || !ownedBy(igw, owner) {
		return nil
	}
	igwID := aws.StringValue(igw.InternetGatewayId)

	if _, err := igwp.DetachInternetGateway(ctx, &ec2.DetachInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
		VpcId:             aws.String(vpcID),
	}, accountID); err != nil {
		slog.WarnContext(ctx, "systemvpc DeleteIGW: detach failed", "igw", igwID, "vpc", vpcID, "err", err)
		return nil
	}
	if _, err := igwp.DeleteInternetGateway(ctx, &ec2.DeleteInternetGatewayInput{
		InternetGatewayId: aws.String(igwID),
	}, accountID); err != nil && !awserrors.IsNotFound(err) {
		slog.WarnContext(ctx, "systemvpc DeleteIGW: delete failed", "igw", igwID, "err", err)
		return nil
	}
	slog.InfoContext(ctx, "systemvpc DeleteIGW: removed IGW", "igw", igwID, "vpc", vpcID, "owner", owner.Name)
	return nil
}

// AttachedIGW returns the Internet Gateway attached to vpcID, or nil if none.
func AttachedIGW(ctx context.Context, igwp IGWProvisioner, accountID, vpcID string) (*ec2.InternetGateway, error) {
	out, err := igwp.DescribeInternetGateways(ctx, &ec2.DescribeInternetGatewaysInput{
		Filters: []*ec2.Filter{{
			Name:   aws.String("attachment.vpc-id"),
			Values: aws.StringSlice([]string{vpcID}),
		}},
	}, accountID)
	if err != nil {
		return nil, fmt.Errorf("systemvpc: describe IGWs for vpc %s: %w", vpcID, err)
	}
	if out == nil || len(out.InternetGateways) == 0 {
		return nil, nil
	}
	return out.InternetGateways[0], nil
}

// ownedBy reports whether igw carries this owner's managed-by + name tags.
func ownedBy(igw *ec2.InternetGateway, owner Owner) bool {
	var managed, named bool
	for _, t := range igw.Tags {
		switch aws.StringValue(t.Key) {
		case tags.ManagedByKey:
			managed = aws.StringValue(t.Value) == owner.ManagedBy
		case owner.OwnerTagKey:
			named = aws.StringValue(t.Value) == owner.Name
		}
	}
	return managed && named
}
