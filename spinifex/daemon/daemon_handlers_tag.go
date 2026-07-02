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

// recordTagMirror is implemented by every service whose resource records
// mirror the central tag store, so tag-filtered describes observe tags
// written through create-tags/delete-tags.
type recordTagMirror interface {
	ApplyRecordTags(input *ec2.CreateTagsInput, accountID string) error
	RemoveRecordTags(input *ec2.DeleteTagsInput, accountID string) error
}

// recordTagMirrors collects the initialized services owning centrally-stored
// records: vpc covers vpc/subnet/sg/eni, the rest own their prefix.
func (d *Daemon) recordTagMirrors() []recordTagMirror {
	var mirrors []recordTagMirror
	if d.vpcService != nil {
		mirrors = append(mirrors, d.vpcService)
	}
	if d.imageService != nil {
		mirrors = append(mirrors, d.imageService)
	}
	if d.volumeService != nil {
		mirrors = append(mirrors, d.volumeService)
	}
	if d.snapshotService != nil {
		mirrors = append(mirrors, d.snapshotService)
	}
	if d.routeTableService != nil {
		mirrors = append(mirrors, d.routeTableService)
	}
	if d.igwService != nil {
		mirrors = append(mirrors, d.igwService)
	}
	if d.eigwService != nil {
		mirrors = append(mirrors, d.eigwService)
	}
	if d.eipService != nil {
		mirrors = append(mirrors, d.eipService)
	}
	if d.natGatewayService != nil {
		mirrors = append(mirrors, d.natGatewayService)
	}
	if d.keyService != nil {
		mirrors = append(mirrors, d.keyService)
	}
	return mirrors
}

// createTags writes the generic tag store, then mirrors tags into the owning
// records so tag-filtered describes observe them.
func (d *Daemon) createTags(input *ec2.CreateTagsInput, accountID string) (*ec2.CreateTagsOutput, error) {
	out, err := d.tagsService.CreateTags(input, accountID)
	if err != nil {
		return nil, err
	}
	for _, m := range d.recordTagMirrors() {
		if err := m.ApplyRecordTags(input, accountID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// deleteTags removes from the generic tag store, then mirrors the removal
// into the owning records.
func (d *Daemon) deleteTags(input *ec2.DeleteTagsInput, accountID string) (*ec2.DeleteTagsOutput, error) {
	out, err := d.tagsService.DeleteTags(input, accountID)
	if err != nil {
		return nil, err
	}
	for _, m := range d.recordTagMirrors() {
		if err := m.RemoveRecordTags(input, accountID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
