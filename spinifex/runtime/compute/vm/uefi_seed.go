package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
)

// ErrUEFIVarStoreExists is a new instance whose store record or claim already
// exists. Seeding never writes over a store it did not create.
var ErrUEFIVarStoreExists = errors.New("UEFI variable store already exists for a new instance")

// uefiClaimTimeout bounds each claim read or unlock a node makes outside a
// launch, so a wedged KV never holds a stop or terminate.
const uefiClaimTimeout = 10 * time.Second

// SeedUEFIVarStore gives a new UEFI instance its variable store: the claim,
// the store record live at the claim's generation, the seed at a fresh key,
// and the pointer moved to it by the fenced write. The seed is image's store
// when image is non-nil and its firmware boots here, else this node's VARS
// template. The launch that follows reuses the local copy. A BIOS guest has none.
func (m *Manager) SeedUEFIVarStore(ctx context.Context, instance *VM, image *uefistore.ImageVarStore) error {
	if !usesUEFIVarStore(instance) {
		return nil
	}
	if m == nil {
		return errors.New("UEFI variable store seeding has no VM manager on this node")
	}
	deps := m.deps.UEFIVarStore
	if deps.Objects == nil || deps.Bucket == "" || deps.DataDir == "" || deps.Records == nil || deps.Claims == nil || m.deps.NodeID == "" {
		return errors.New("UEFI variable store seeding is not wired on this node")
	}
	if m.deps.InstanceTypes == nil {
		return errors.New("InstanceTypeResolver not wired")
	}
	spec, ok := m.deps.InstanceTypes.Resolve(instance.InstanceType)
	if !ok {
		return fmt.Errorf("instance type %s not found", instance.InstanceType)
	}
	fw, err := NodeFirmwareFor(spec.Architecture)
	if err != nil {
		return err
	}
	dir, err := uefiInstanceDir(deps.DataDir, instance.ID)
	if err != nil {
		return err
	}
	// Until the record is created, any store under this ID belongs to
	// something else, which a failed RunInstances must not delete.
	m.Inspect(instance, func(v *VM) { v.UEFIStoreUnowned = true })
	seed, source, err := m.uefiSeedRegion(ctx, instance, image, fw)
	if err != nil {
		return err
	}

	// A held claim on a brand-new ID is not this launch's, so it is left alone.
	claim := deps.Claims.Get(instance.ID)
	if holder, held, err := claim.Holder(ctx); err != nil {
		return fmt.Errorf("read UEFI store claim for %s: %w", instance.ID, err)
	} else if held {
		return fmt.Errorf("%w: %s is claimed by %s at generation %d", ErrUEFIVarStoreExists, instance.ID, holder.Holder, holder.Generation)
	}
	tok, err := claim.TryLock(ctx)
	if err != nil {
		return fmt.Errorf("take UEFI store claim for %s: %w", instance.ID, err)
	}
	if _, err := deps.Records.Create(ctx, &uefistore.UEFIStoreRecord{
		InstanceID: instance.ID,
		AccountID:  instance.AccountID,
		State:      uefistore.StateLive,
		Generation: tok.Generation,
	}); err != nil {
		m.unlockUEFIClaim(ctx, instance.ID, tok, "the store record could not be created")
		if errors.Is(err, kvstore.ErrExists) {
			return fmt.Errorf("%w: %s has a store record", ErrUEFIVarStoreExists, instance.ID)
		}
		return fmt.Errorf("create UEFI store record for %s: %w", instance.ID, err)
	}

	// From here the record exists, so terminate reaches every object put
	// under the prefix, and a failed RunInstances terminates the instance.
	m.Inspect(instance, func(v *VM) { v.UEFIStoreUnowned = false })
	start := time.Now()
	key, err := uefistore.NewObjectKey(instance.ID)
	if err != nil {
		return err
	}
	if err := uefistore.PutObject(ctx, deps.Objects, deps.Bucket, key, seed, uefistore.ObjectMeta{
		OwnerID: instance.ID, WriterNode: m.deps.NodeID, Firmware: fw.Identity, CodePath: fw.Code, Source: source,
	}); err != nil {
		return err
	}
	ref := uefistore.UEFIVarStoreRef{Key: key, Firmware: fw.Identity, WriterNode: m.deps.NodeID, WrittenAt: time.Now().UTC()}
	if err := deps.Records.MovePointer(ctx, instance.ID, tok.Generation, ref); err != nil {
		return fmt.Errorf("point UEFI store record for %s at %s: %w", instance.ID, key, err)
	}

	// Only saves the launch a get: without the copy it fetches the object.
	if err := writeFileAtomic(dir, uefiVarsFileName, seed); err != nil {
		slog.WarnContext(ctx, "Failed to keep the seeded UEFI variable store locally; the launch fetches it",
			"instanceId", instance.ID, "err", err)
	} else if err := writeUEFISeed(dir, uefiSeed{
		Key: key, Generation: tok.Generation, SeededAt: time.Now().UTC(), Firmware: fw.Identity, CodePath: fw.Code,
		Sum: uefiRegionSum(seed),
	}); err != nil {
		slog.WarnContext(ctx, "Failed to record the seeded UEFI variable store locally; the launch fetches it",
			"instanceId", instance.ID, "err", err)
	}
	slog.InfoContext(ctx, "Seeded the UEFI variable store", "instanceId", instance.ID, "key", key,
		"source", source, "generation", tok.Generation, "arch", fw.Identity.Arch,
		"varsSize", fw.Identity.VarsSize, "put_ms", otelsetup.Millis(time.Since(start)))
	return nil
}

// uefiSeedRegion returns the region a new instance's store starts from and
// its source. image's store is read before anything is claimed or recorded;
// one whose arch or VARS size cannot boot here gives way to the template.
func (m *Manager) uefiSeedRegion(ctx context.Context, instance *VM, image *uefistore.ImageVarStore, fw NodeFirmware) ([]byte, string, error) {
	if image != nil {
		stored, node := image.Firmware, fw.Identity
		if stored.Arch == node.Arch && stored.VarsSize == node.VarsSize {
			if stored.CodeSHA256 != node.CodeSHA256 {
				recordUEFIFirmwareMismatch("code")
				slog.WarnContext(ctx, "The image's UEFI variable store was written under a different CODE build; seeding from it",
					"instanceId", instance.ID, "imageId", image.ImageID, "key", image.Key,
					"storeCodeSha256", stored.CodeSHA256, "nodeCodeSha256", node.CodeSHA256, "nodeCodePath", fw.Code)
			}
			region, _, err := uefistore.ReadObject(ctx, m.deps.UEFIVarStore.Objects, m.deps.UEFIVarStore.Bucket, image.Key, node.VarsSize)
			if err != nil {
				return nil, "", fmt.Errorf("read the UEFI variable store of %s: %w", image.ImageID, err)
			}
			return region, uefistore.SourceImage(image.ImageID), nil
		}
		recordUEFIFirmwareMismatch("image")
		slog.WarnContext(ctx, "The image's UEFI variable store cannot boot under this node's firmware; seeding from the VARS template",
			"instanceId", instance.ID, "imageId", image.ImageID, "key", image.Key,
			"storeArch", stored.Arch, "storeVarsSize", stored.VarsSize,
			"nodeArch", node.Arch, "nodeVarsSize", node.VarsSize, "nodeCodePath", fw.Code)
	}
	template, err := os.ReadFile(fw.VarsTemplate)
	if err != nil {
		return nil, "", fmt.Errorf("read VARS template %s: %w", fw.VarsTemplate, err)
	}
	if int64(len(template)) != fw.Identity.VarsSize {
		return nil, "", fmt.Errorf("VARS template %s is %d bytes, and was %d when stat'd", fw.VarsTemplate, len(template), fw.Identity.VarsSize)
	}
	return template, uefistore.SourceTemplate, nil
}
