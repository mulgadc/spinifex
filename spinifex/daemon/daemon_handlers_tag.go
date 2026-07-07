package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/types"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
)

// instanceTagCommandTimeout bounds each ec2.cmd owner request. A stopped or
// absent instance has no subscriber and fails fast with no responders, so
// only a partitioned-but-subscribed owner pays the full timeout.
const instanceTagCommandTimeout = 5 * time.Second

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
	if m, ok := d.eipService.(recordTagMirror); ok {
		mirrors = append(mirrors, m)
	}
	if d.natGatewayService != nil {
		mirrors = append(mirrors, d.natGatewayService)
	}
	if d.keyService != nil {
		mirrors = append(mirrors, d.keyService)
	}
	return mirrors
}

// splitInstanceResources separates instance IDs from the other resource IDs.
// Instances never take the generic central write: their central entry is only
// written by the ownership-gated path co-located with the record write.
func splitInstanceResources(resources []*string) (instanceIDs []string, rest []*string) {
	for _, r := range resources {
		if r == nil {
			continue
		}
		if strings.HasPrefix(*r, "i-") {
			instanceIDs = append(instanceIDs, *r)
		} else {
			rest = append(rest, r)
		}
	}
	return instanceIDs, rest
}

// createTags routes instance IDs to their owning daemon, which writes the
// record and the central store together, and writes everything else to the
// generic tag store plus the owning record mirrors.
func (d *Daemon) createTags(ctx context.Context, input *ec2.CreateTagsInput, accountID string) (*ec2.CreateTagsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.Resources) == 0 || len(input.Tags) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	instanceIDs, rest := splitInstanceResources(input.Resources)
	if len(rest) > 0 {
		restInput := *input
		restInput.Resources = rest
		if _, err := d.tagsService.CreateTags(ctx, &restInput, accountID); err != nil {
			return nil, err
		}
		for _, m := range d.recordTagMirrors() {
			if err := m.ApplyRecordTags(&restInput, accountID); err != nil {
				return nil, err
			}
		}
	}

	data := &types.InstanceTagsData{Tags: make(map[string]string, len(input.Tags))}
	for _, tag := range input.Tags {
		if tag != nil && tag.Key != nil && tag.Value != nil {
			data.Tags[*tag.Key] = *tag.Value
		}
	}
	if err := d.tagInstances(ctx, instanceIDs, data, false, accountID); err != nil {
		return nil, err
	}
	return &ec2.CreateTagsOutput{}, nil
}

// deleteTags is createTags' removal counterpart: instances go via their owner,
// the rest via the generic tag store plus record mirrors.
func (d *Daemon) deleteTags(ctx context.Context, input *ec2.DeleteTagsInput, accountID string) (*ec2.DeleteTagsOutput, error) {
	if input == nil {
		return nil, errors.New(awserrors.ErrorInvalidParameterValue)
	}
	if len(input.Resources) == 0 {
		return nil, errors.New(awserrors.ErrorMissingParameter)
	}

	instanceIDs, rest := splitInstanceResources(input.Resources)
	if len(rest) > 0 {
		restInput := *input
		restInput.Resources = rest
		if _, err := d.tagsService.DeleteTags(ctx, &restInput, accountID); err != nil {
			return nil, err
		}
		for _, m := range d.recordTagMirrors() {
			if err := m.RemoveRecordTags(&restInput, accountID); err != nil {
				return nil, err
			}
		}
	}

	// Keys without a value delete unconditionally; keys with a value delete
	// only on match; an empty input.Tags clears every tag.
	data := &types.InstanceTagsData{}
	for _, tag := range input.Tags {
		if tag == nil || tag.Key == nil {
			continue
		}
		if tag.Value == nil {
			data.TagKeys = append(data.TagKeys, *tag.Key)
			continue
		}
		if data.Tags == nil {
			data.Tags = make(map[string]string)
		}
		data.Tags[*tag.Key] = *tag.Value
	}
	if err := d.tagInstances(ctx, instanceIDs, data, true, accountID); err != nil {
		return nil, err
	}
	return &ec2.DeleteTagsOutput{}, nil
}

// tagInstances sends the tag mutation to each instance's owner concurrently,
// so a many-instance call costs one owner timeout rather than one per
// instance. The first failure in input order is returned.
func (d *Daemon) tagInstances(ctx context.Context, instanceIDs []string, data *types.InstanceTagsData, remove bool, accountID string) error {
	errs := make([]error, len(instanceIDs))
	var wg sync.WaitGroup
	for i, id := range instanceIDs {
		wg.Go(func() {
			errs[i] = d.tagInstance(ctx, id, data, remove, accountID)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// tagInstance asks the owning daemon over ec2.cmd.<id> to apply the mutation
// to the record and central store together; only the owner subscribes there.
// No responders means the instance isn't running, so the mutation falls back
// to the shared stopped store; a timeout (partitioned-but-subscribed owner)
// surfaces InvalidID.NotFound and writes nothing.
func (d *Daemon) tagInstance(ctx context.Context, instanceID string, data *types.InstanceTagsData, remove bool, accountID string) error {
	command := types.EC2InstanceCommand{
		ID: instanceID,
		Attributes: types.EC2CommandAttributes{
			SetInstanceTags:    !remove,
			RemoveInstanceTags: remove,
		},
		InstanceTagsData: data,
	}
	body, err := json.Marshal(command)
	if err != nil {
		slog.ErrorContext(ctx, "tagInstance: failed to marshal command", "instanceId", instanceID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	reqMsg := nats.NewMsg("ec2.cmd." + instanceID)
	reqMsg.Data = body
	reqMsg.Header.Set(utils.AccountIDHeader, accountID)

	msg, err := d.natsConn.RequestMsg(reqMsg, instanceTagCommandTimeout)
	switch {
	case errors.Is(err, nats.ErrNoResponders):
		return d.instanceService.TagStoppedInstance(ctx, instanceID, data, remove, d.tagsService, accountID)
	case errors.Is(err, nats.ErrTimeout):
		return errors.New(awserrors.ErrorInvalidInstanceIDNotFound)
	case err != nil:
		slog.ErrorContext(ctx, "tagInstance: owner request failed", "instanceId", instanceID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	if responseError, parseErr := utils.ValidateErrorPayload(msg.Data); parseErr != nil {
		return errors.New(*responseError.Code)
	}
	return nil
}
