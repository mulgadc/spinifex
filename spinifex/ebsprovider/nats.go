package ebsprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

const (
	CapabilitiesSubject   = "ebs.provider.v1.capabilities"
	CreateVolumeSubject   = "ebs.provider.v1.volume.create"
	GetVolumeSubject      = "ebs.provider.v1.volume.describe"
	ExpandVolumeSubject   = "ebs.provider.v1.volume.expand"
	DeleteVolumeSubject   = "ebs.provider.v1.volume.delete"
	DeleteSnapshotSubject = "ebs.provider.v1.snapshot.delete"
)

const defaultRequestTimeout = 30 * time.Second

func SnapshotSubject(volumeID string) (string, error) {
	if err := validateSubjectToken(volumeID); err != nil {
		return "", err
	}
	return "ebs.provider.v1.snapshot." + volumeID, nil
}

func SnapshotCompletionSubject(snapshotID string) (string, error) {
	if err := validateSubjectToken(snapshotID); err != nil {
		return "", err
	}
	return "ebs.provider.v1.snapshot.response." + snapshotID, nil
}

func PublishSubject(nodeID string) (string, error) {
	if err := validateSubjectToken(nodeID); err != nil {
		return "", err
	}
	return "ebs.provider.v1." + nodeID + ".mount", nil
}

func UnpublishSubject(nodeID string) (string, error) {
	if err := validateSubjectToken(nodeID); err != nil {
		return "", err
	}
	return "ebs.provider.v1." + nodeID + ".unmount", nil
}

func validateSubjectToken(value string) error {
	if value == "" || strings.ContainsAny(value, ".*>") {
		return fmt.Errorf("%w: invalid NATS subject token %q", ErrInvalidArgument, value)
	}
	return nil
}

// NATSProvider drives a provider daemon through the versioned ebs.* contract.
type NATSProvider struct {
	conn           *nats.Conn
	requestTimeout time.Duration
}

var _ EBSProvider = (*NATSProvider)(nil)

func NewNATSProvider(conn *nats.Conn, requestTimeout time.Duration) *NATSProvider {
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	return &NATSProvider{conn: conn, requestTimeout: requestTimeout}
}

func (p *NATSProvider) GetCapabilities(ctx context.Context, req GetCapabilitiesRequest) (*GetCapabilitiesResponse, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	var response GetCapabilitiesResponse
	if err := p.request(ctx, CapabilitiesSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	return &response, nil
}

func (p *NATSProvider) CreateVolume(ctx context.Context, req CreateVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	var response CreateVolumeResponse
	if err := p.request(ctx, CreateVolumeSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Volume == nil {
		return nil, fmt.Errorf("ebs.create returned no volume")
	}
	return response.Volume, nil
}

func (p *NATSProvider) GetVolume(ctx context.Context, req GetVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	var response GetVolumeResponse
	if err := p.request(ctx, GetVolumeSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Volume == nil {
		return nil, fmt.Errorf("ebs.describe returned no volume")
	}
	return response.Volume, nil
}

func (p *NATSProvider) ExpandVolume(ctx context.Context, req ExpandVolumeRequest) (*Volume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	var response ExpandVolumeResponse
	if err := p.request(ctx, ExpandVolumeSubject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Volume == nil {
		return nil, fmt.Errorf("ebs.expand returned no volume")
	}
	return response.Volume, nil
}

func (p *NATSProvider) DeleteVolume(ctx context.Context, req DeleteVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	var response DeleteVolumeResponse
	if err := p.request(ctx, DeleteVolumeSubject, req, &response); err != nil {
		return err
	}
	return responseError(response.SchemaVersion, response.Error)
}

func (p *NATSProvider) CreateSnapshot(ctx context.Context, req CreateSnapshotRequest) (*Snapshot, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	requestSubject, err := SnapshotSubject(req.VolumeID)
	if err != nil {
		return nil, err
	}
	completionSubject, err := SnapshotCompletionSubject(req.SnapshotID)
	if err != nil {
		return nil, err
	}
	if p.conn == nil || !p.conn.IsConnected() {
		return nil, nats.ErrConnectionClosed
	}

	completionSub, err := p.conn.SubscribeSync(completionSubject)
	if err != nil {
		return nil, fmt.Errorf("subscribe to snapshot completion: %w", err)
	}
	defer completionSub.Unsubscribe()
	if err := p.conn.Flush(); err != nil {
		return nil, fmt.Errorf("flush snapshot completion subscription: %w", err)
	}

	var accepted CreateSnapshotResponse
	if err := p.request(ctx, requestSubject, req, &accepted); err != nil {
		return nil, err
	}
	if err := responseError(accepted.SchemaVersion, accepted.Error); err != nil {
		return nil, err
	}
	if accepted.Snapshot != nil && accepted.Snapshot.State != SnapshotStatePending {
		return accepted.Snapshot, nil
	}
	if accepted.OperationID == "" {
		return nil, fmt.Errorf("%s returned no operation ID", requestSubject)
	}
	if accepted.CompletionSubject != "" && accepted.CompletionSubject != completionSubject {
		return nil, fmt.Errorf("snapshot completion subject mismatch: got %q, want %q", accepted.CompletionSubject, completionSubject)
	}

	msg, err := completionSub.NextMsgWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("wait for snapshot %s completion: %w", req.SnapshotID, err)
	}
	var completed CreateSnapshotResponse
	if err := json.Unmarshal(msg.Data, &completed); err != nil {
		return nil, fmt.Errorf("decode snapshot completion: %w", err)
	}
	if err := responseError(completed.SchemaVersion, completed.Error); err != nil {
		return nil, err
	}
	if completed.OperationID != accepted.OperationID {
		return nil, fmt.Errorf("snapshot operation mismatch: got %q, want %q", completed.OperationID, accepted.OperationID)
	}
	if completed.Snapshot == nil || completed.Snapshot.State != SnapshotStateCompleted {
		return nil, fmt.Errorf("snapshot %s completion returned no completed snapshot", req.SnapshotID)
	}
	return completed.Snapshot, nil
}

func (p *NATSProvider) DeleteSnapshot(ctx context.Context, req DeleteSnapshotRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	var response DeleteSnapshotResponse
	if err := p.request(ctx, DeleteSnapshotSubject, req, &response); err != nil {
		return err
	}
	return responseError(response.SchemaVersion, response.Error)
}

func (p *NATSProvider) PublishVolume(ctx context.Context, req PublishVolumeRequest) (*PublishedVolume, error) {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return nil, err
	}
	subject, err := PublishSubject(req.NodeID)
	if err != nil {
		return nil, err
	}
	var response PublishVolumeResponse
	if err := p.request(ctx, subject, req, &response); err != nil {
		return nil, err
	}
	if err := responseError(response.SchemaVersion, response.Error); err != nil {
		return nil, err
	}
	if response.Published == nil {
		return nil, fmt.Errorf("%s returned no published volume", subject)
	}
	return response.Published, nil
}

func (p *NATSProvider) UnpublishVolume(ctx context.Context, req UnpublishVolumeRequest) error {
	if err := checkVersion(req.SchemaVersion); err != nil {
		return err
	}
	subject, err := UnpublishSubject(req.NodeID)
	if err != nil {
		return err
	}
	var response UnpublishVolumeResponse
	if err := p.request(ctx, subject, req, &response); err != nil {
		return err
	}
	return responseError(response.SchemaVersion, response.Error)
}

func (p *NATSProvider) request(ctx context.Context, subject string, input, output any) error {
	if p.conn == nil || !p.conn.IsConnected() {
		return nats.ErrConnectionClosed
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", subject, err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, p.requestTimeout)
	defer cancel()
	msg, err := p.conn.RequestWithContext(requestCtx, subject, payload)
	if err != nil {
		return fmt.Errorf("request %s: %w", subject, err)
	}
	if err := json.Unmarshal(msg.Data, output); err != nil {
		return fmt.Errorf("decode %s response: %w", subject, err)
	}
	return nil
}

func responseError(version uint16, providerErr *ProviderError) error {
	if err := checkVersion(version); err != nil {
		return err
	}
	if providerErr != nil {
		return providerErr
	}
	return nil
}
