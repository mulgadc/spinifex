package daemon

import (
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/nats-io/nats.go"
)

func (d *Daemon) handleEC2CreateTags(msg *nats.Msg) {
	handleNATSRequest(msg, d.createTags)
}

func (d *Daemon) handleEC2DeleteTags(msg *nats.Msg) {
	handleNATSRequest(msg, d.deleteTags)
}

func (d *Daemon) handleEC2DescribeTags(msg *nats.Msg) {
	handleNATSRequest(msg, d.tagsService.DescribeTags)
}

// createTags writes the generic tag store, then mirrors subnet/vpc tags into
// their owning records so tag-filtered describes (e.g. LBC subnet
// auto-discovery via DescribeSubnets) observe them.
func (d *Daemon) createTags(input *ec2.CreateTagsInput, accountID string) (*ec2.CreateTagsOutput, error) {
	out, err := d.tagsService.CreateTags(input, accountID)
	if err != nil {
		return nil, err
	}
	if d.vpcService != nil {
		if err := d.vpcService.ApplyRecordTags(input, accountID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// deleteTags removes from the generic tag store, then mirrors the removal into
// the owning subnet/vpc records.
func (d *Daemon) deleteTags(input *ec2.DeleteTagsInput, accountID string) (*ec2.DeleteTagsOutput, error) {
	out, err := d.tagsService.DeleteTags(input, accountID)
	if err != nil {
		return nil, err
	}
	if d.vpcService != nil {
		if err := d.vpcService.RemoveRecordTags(input, accountID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
