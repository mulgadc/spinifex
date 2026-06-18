package handlers_ec2_image

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// NATSImageService handles image operations via NATS messaging
type NATSImageService struct {
	natsConn      *nats.Conn
	expectedNodes int
}

// NewNATSImageService creates a new NATS-based image service.
// expectedNodes is used by scatter-gather operations (e.g. CreateImage) to
// enable early exit once all nodes have responded.
func NewNATSImageService(conn *nats.Conn, expectedNodes int) ImageService {
	return &NATSImageService{natsConn: conn, expectedNodes: expectedNodes}
}

func (s *NATSImageService) DescribeImages(input *ec2.DescribeImagesInput, accountID string) (*ec2.DescribeImagesOutput, error) {
	return utils.NATSRequest[ec2.DescribeImagesOutput](s.natsConn, "ec2.DescribeImages", input, 30*time.Second, accountID)
}

func (s *NATSImageService) CreateImage(input *ec2.CreateImageInput, accountID string) (*ec2.CreateImageOutput, error) {
	jsonData, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal input: %w", err)
	}

	frames, sum, err := utils.Gather(s.natsConn, "ec2.CreateImage", jsonData,
		utils.GatherOpts{Timeout: 120 * time.Second, ExpectedNodes: s.expectedNodes, StopOnFirst: true, AccountID: accountID})
	if err != nil {
		return nil, err
	}

	if len(frames) > 0 {
		var out ec2.CreateImageOutput
		if err := json.Unmarshal(frames[0], &out); err != nil {
			return nil, fmt.Errorf("failed to unmarshal CreateImage response: %w", err)
		}
		return &out, nil
	}

	// No node produced the image: surface the first deterministic client error
	// (e.g. the owner is absent and every node replied NotFound), else a timeout.
	if sum.FirstClient4xx != "" {
		return nil, errors.New(sum.FirstClient4xx)
	}
	return nil, fmt.Errorf("CreateImage: no successful response received for ec2.CreateImage")
}

func (s *NATSImageService) CopyImage(input *ec2.CopyImageInput, accountID string) (*ec2.CopyImageOutput, error) {
	return utils.NATSRequest[ec2.CopyImageOutput](s.natsConn, "ec2.CopyImage", input, 30*time.Second, accountID)
}

func (s *NATSImageService) DescribeImageAttribute(input *ec2.DescribeImageAttributeInput, accountID string) (*ec2.DescribeImageAttributeOutput, error) {
	return utils.NATSRequest[ec2.DescribeImageAttributeOutput](s.natsConn, "ec2.DescribeImageAttribute", input, 30*time.Second, accountID)
}

func (s *NATSImageService) RegisterImage(input *ec2.RegisterImageInput, accountID string) (*ec2.RegisterImageOutput, error) {
	return utils.NATSRequest[ec2.RegisterImageOutput](s.natsConn, "ec2.RegisterImage", input, 30*time.Second, accountID)
}

func (s *NATSImageService) DeregisterImage(input *ec2.DeregisterImageInput, accountID string) (*ec2.DeregisterImageOutput, error) {
	return utils.NATSRequest[ec2.DeregisterImageOutput](s.natsConn, "ec2.DeregisterImage", input, 30*time.Second, accountID)
}

func (s *NATSImageService) ModifyImageAttribute(input *ec2.ModifyImageAttributeInput, accountID string) (*ec2.ModifyImageAttributeOutput, error) {
	return utils.NATSRequest[ec2.ModifyImageAttributeOutput](s.natsConn, "ec2.ModifyImageAttribute", input, 30*time.Second, accountID)
}

func (s *NATSImageService) ResetImageAttribute(input *ec2.ResetImageAttributeInput, accountID string) (*ec2.ResetImageAttributeOutput, error) {
	return utils.NATSRequest[ec2.ResetImageAttributeOutput](s.natsConn, "ec2.ResetImageAttribute", input, 30*time.Second, accountID)
}
