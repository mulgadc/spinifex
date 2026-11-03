package instance

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
)

// UEFIStoreRecords creates the store record that owns a new instance's
// variable store objects.
type UEFIStoreRecords interface {
	Create(ctx context.Context, rec *uefistore.UEFIStoreRecord) (uint64, error)
}

var _ UEFIStoreRecords = (*uefistore.Store)(nil)

// SetUEFIVarStore wires where RunInstances records and writes a new UEFI
// instance's variable store, and the node named as its writer.
func (s *InstanceServiceImpl) SetUEFIVarStore(records UEFIStoreRecords, node string) {
	s.uefiStores = records
	s.uefiNode = node
}

// seedUEFIVarStore gives a new UEFI instance its variable store: the store
// record, live, then the seed object at a fresh key, then the pointer on the
// in-memory instance, which the launch's record writes carry.
func (s *InstanceServiceImpl) seedUEFIVarStore(ctx context.Context, instance *vm.VM, imageID, arch string) error {
	bucket := ""
	if s.config != nil {
		bucket = s.config.Predastore.Bucket
	}
	if s.uefiStores == nil || s.objectStore == nil || bucket == "" || s.uefiNode == "" {
		slog.ErrorContext(ctx, "UEFI variable store seeding is not wired on this node", "instanceId", instance.ID)
		return errors.New(awserrors.ErrorServerInternal)
	}
	fw, err := vm.NodeFirmwareFor(arch)
	if err != nil {
		slog.ErrorContext(ctx, "UEFI firmware not installed on this host", "instanceId", instance.ID, "arch", arch, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	region, source, err := s.uefiSeedRegion(ctx, imageID, fw)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to read the UEFI variable store seed", "instanceId", instance.ID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}
	key, err := uefistore.NewObjectKey(instance.ID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to name the UEFI variable store object", "instanceId", instance.ID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	// The record first, so terminate can reach every object under the prefix.
	if _, err := s.uefiStores.Create(ctx, &uefistore.UEFIStoreRecord{
		InstanceID: instance.ID,
		AccountID:  instance.AccountID,
		State:      uefistore.StateLive,
	}); err != nil {
		slog.ErrorContext(ctx, "Failed to create the UEFI store record", "instanceId", instance.ID, "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	start := time.Now()
	if err := uefistore.PutObject(ctx, s.objectStore, bucket, key, region, uefistore.ObjectMeta{
		OwnerID:    instance.ID,
		WriterNode: s.uefiNode,
		Firmware:   fw.Identity,
		CodePath:   fw.Code,
		Source:     source,
	}); err != nil {
		slog.ErrorContext(ctx, "Failed to write the UEFI variable store seed", "instanceId", instance.ID, "key", key,
			"put_ms", otelsetup.Millis(time.Since(start)), "err", err)
		return errors.New(awserrors.ErrorServerInternal)
	}

	ref := &vm.UEFIVarStoreRef{Key: key, Firmware: fw.Identity, WriterNode: s.uefiNode, WrittenAt: time.Now().UTC()}
	s.vmMgr.Inspect(instance, func(v *vm.VM) { v.UEFIVarStore = ref })
	slog.InfoContext(ctx, "Seeded the UEFI variable store", "instanceId", instance.ID, "key", key, "source", source,
		"arch", fw.Identity.Arch, "varsSize", fw.Identity.VarsSize, "put_ms", otelsetup.Millis(time.Since(start)))
	return nil
}

// uefiSeedRegion returns the region a new instance's store starts from and
// the source its object records. AMI documents name no variable store yet,
// so every launch seeds from this node's VARS template.
func (s *InstanceServiceImpl) uefiSeedRegion(_ context.Context, _ string, fw vm.NodeFirmware) ([]byte, string, error) {
	region, err := os.ReadFile(fw.VarsTemplate)
	if err != nil {
		return nil, "", err
	}
	return region, uefistore.SourceTemplate, nil
}
