package accountlifecycle

import (
	"context"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	ec2tgw "github.com/mulgadc/spinifex/spinifex/domains/ec2/tgw"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/nats-io/nats.go"
)

// TransitGatewayReapers returns the network-stage reapers for Transit
// Gateways: attachments first, because a live attachment refuses its
// gateway's delete, and then the gateways.
func TransitGatewayReapers(nc *nats.Conn) []Reaper {
	svc := ec2tgw.NewNATSTransitGatewayService(nc)
	return []Reaper{&tgwAttachmentReaper{svc: svc}, &transitGatewayReaper{svc: svc}}
}

// finalRereader is a reaper whose kind a create admitted before the quiesce
// can add after its stage drained, so the engine lists it again immediately
// before it removes the account record.
type finalRereader interface {
	Reaper
	rereadBeforeDelete()
}

// notOffered is a formation below the Transit Gateway level, which holds no
// Transit Gateway: its rollback waits until none is left.
func notOffered(err error) bool {
	return err != nil && strings.Contains(err.Error(), awserrors.ErrorUnsupportedOperation)
}

type tgwAttachmentReaper struct {
	svc ec2tgw.TransitGatewayService
}

var _ finalRereader = (*tgwAttachmentReaper)(nil)

func (r *tgwAttachmentReaper) Kind() string        { return "transit-gateway-attachment" }
func (r *tgwAttachmentReaper) Stage() Stage        { return StageNetwork }
func (r *tgwAttachmentReaper) rereadBeforeDelete() {}

// List keeps an attachment that is deleting: it still blocks its VPC and its
// gateway until it is deleted.
func (r *tgwAttachmentReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	var found []Resource
	input := &ec2.DescribeTransitGatewayVpcAttachmentsInput{}
	for {
		out, err := r.svc.DescribeTransitGatewayVpcAttachments(ctx, input, accountID)
		if notOffered(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, a := range out.TransitGatewayVpcAttachments {
			if a == nil || a.TransitGatewayAttachmentId == nil {
				continue
			}
			state := aws.StringValue(a.State)
			if state == ec2tgw.StateDeleted || state == ec2tgw.StateFailed {
				continue
			}
			found = append(found, Resource{Kind: r.Kind(), ID: aws.StringValue(a.TransitGatewayAttachmentId), Detail: state})
		}
		if aws.StringValue(out.NextToken) == "" {
			return found, nil
		}
		input.NextToken = out.NextToken
	}
}

func (r *tgwAttachmentReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteTransitGatewayVpcAttachment(ctx, &ec2.DeleteTransitGatewayVpcAttachmentInput{
		TransitGatewayAttachmentId: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}

type transitGatewayReaper struct {
	svc ec2tgw.TransitGatewayService
}

var _ finalRereader = (*transitGatewayReaper)(nil)

func (r *transitGatewayReaper) Kind() string        { return "transit-gateway" }
func (r *transitGatewayReaper) Stage() Stage        { return StageNetwork }
func (r *transitGatewayReaper) rereadBeforeDelete() {}

func (r *transitGatewayReaper) List(ctx context.Context, accountID string) ([]Resource, error) {
	var found []Resource
	input := &ec2.DescribeTransitGatewaysInput{}
	for {
		out, err := r.svc.DescribeTransitGateways(ctx, input, accountID)
		if notOffered(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, g := range out.TransitGateways {
			if g == nil || g.TransitGatewayId == nil || aws.StringValue(g.State) == ec2tgw.StateDeleted {
				continue
			}
			found = append(found, Resource{Kind: r.Kind(), ID: aws.StringValue(g.TransitGatewayId), Detail: aws.StringValue(g.State)})
		}
		if aws.StringValue(out.NextToken) == "" {
			return found, nil
		}
		input.NextToken = out.NextToken
	}
}

// Delete is refused with IncorrectState while an attachment is live; the
// next pass retries once the attachment reaper has removed it.
func (r *transitGatewayReaper) Delete(ctx context.Context, accountID string, resource Resource, _ bool) error {
	_, err := r.svc.DeleteTransitGateway(ctx, &ec2.DeleteTransitGatewayInput{
		TransitGatewayId: aws.String(resource.ID),
	}, accountID)
	return ignoreAlreadyGone(err)
}
