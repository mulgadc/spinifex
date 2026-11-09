package vm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	clusterv1 "github.com/mulgadc/spinifex/contracts/cluster/v1"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	otelsetup "github.com/mulgadc/spinifex/spinifex/foundation/telemetry"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	// uefiStoreDirName is the directory under DataDir that holds one
	// subdirectory per instance with its live variable store.
	uefiStoreDirName   = "uefi"
	uefiVarsFileName   = "vars.fd"
	uefiSeededFileName = "seeded.json"

	// uefiStoreGetTimeout bounds the one get a launch makes for a fixed-size
	// object, so a wedged object store fails the start rather than holding it.
	uefiStoreGetTimeout = 10 * time.Second

	// uefiClaimReleaseTimeout bounds the unlock a rolled-back launch makes.
	uefiClaimReleaseTimeout = 10 * time.Second

	// uefiStoreBodySlack is how far a compressed body may exceed its region
	// before it is refused; gzip grows incompressible data by far less.
	uefiStoreBodySlack = 1 << 20

	// uefiStateReasonCode is the StateReason a launch refused over its
	// variable store records, as EC2 reports an internal launch failure.
	uefiStateReasonCode = "Server.InternalError"
)

// ErrUEFIVarStoreMissing is a UEFI instance whose store record is absent,
// deleting or names no pointer, so it has nothing to boot its variables from.
var ErrUEFIVarStoreMissing = errors.New("UEFI instance has no variable store")

// ErrUEFIEFIVolume is a UEFI instance that still carries a <root>-efi volume,
// which no node boots: the cutover converts every one into a store record.
var ErrUEFIEFIVolume = errors.New("UEFI instance still carries an EFI volume")

// ErrUEFIFirmwareMismatch is a variable store written under firmware whose
// architecture or VARS size differs from this node's, which pflash cannot load.
var ErrUEFIFirmwareMismatch = errors.New("UEFI variable store firmware does not match this node")

// ErrUEFIVarStoreUnusable is a store record this node cannot trust, or a store
// object that is missing or does not decode to a region of this node's VARS
// size. Retrying does not help.
var ErrUEFIVarStoreUnusable = errors.New("UEFI variable store object is unusable")

// ErrUEFIClaimHeld is a start refused because another node holds the store's
// claim and its heartbeat is fresh or cannot be read: that node may still land
// newer variables. It always wraps ErrMountRetryable.
var ErrUEFIClaimHeld = errors.New("UEFI variable store is claimed by another node that may be live")

// UEFIStoreRecords is the store-record surface a node creates, stamps and
// moves the pointer of.
type UEFIStoreRecords interface {
	Get(ctx context.Context, instanceID string) (*uefistore.UEFIStoreRecord, uint64, error)
	LeaderGet(ctx context.Context, instanceID string) (*uefistore.UEFIStoreRecord, uint64, error)
	Create(ctx context.Context, rec *uefistore.UEFIStoreRecord) (uint64, error)
	Stamp(ctx context.Context, instanceID string, generation uint64) (*uefistore.UEFIStoreRecord, error)
	MovePointer(ctx context.Context, instanceID string, generation uint64, ref uefistore.UEFIVarStoreRef) error
}

var _ UEFIStoreRecords = (*uefistore.Store)(nil)

// NodeLiveness reports when a node last heartbeated. found is false only for
// a node with no heartbeat. A heartbeat that exists but cannot be dated is an
// error, as is a failed read: the answer is unknown, and never reads as dead.
type NodeLiveness interface {
	LastSeen(ctx context.Context, node string) (seen time.Time, found bool, err error)
}

// UEFIVarStoreDeps is where a node reads instance variable stores from and
// keeps the live copy QEMU opens. DataDir is the daemon's persistent data
// directory, so the copy survives a daemon restart and a host reboot.
type UEFIVarStoreDeps struct {
	Objects objectstore.ObjectStore
	Bucket  string
	DataDir string
	// Records holds each store's pointer and the claim generation fencing it.
	Records UEFIStoreRecords
	// Claims is this node's group of store claims; Liveness decides whether
	// the holder of a claim this node finds held may be taken over.
	Claims   *kvsync.ClaimGroup
	Liveness NodeLiveness
	// Clock times the write-back's windows and the prune's age; nil is real time.
	Clock UEFIClock
}

// uefiSeed is seeded.json: the pointer key vars.fd beside it was seeded from
// or last written back to, the claim generation it was written under, and the
// firmware it runs under, so a write-back can name it without the instance.
type uefiSeed struct {
	Key        string                     `json:"key"`
	Generation uint64                     `json:"generation"`
	SeededAt   time.Time                  `json:"seeded_at"`
	Firmware   uefistore.FirmwareIdentity `json:"firmware"`
	CodePath   string                     `json:"code_path,omitempty"`
	// WriteBackOwed is set while vars.fd may hold changes no object has.
	WriteBackOwed bool `json:"write_back_owed,omitempty"`
	// Sum is the SHA-256 of the region Key holds, so a stop that missed a
	// write event still sees vars.fd differ. Empty is unknown.
	Sum string `json:"sum,omitempty"`
}

// uefiClaim is the claim a launch acquired. reused is set when the launch
// boots this node's copy. keep is set when this node held a copy under token
// before the launch, which may carry changes no object has, so a rollback
// must not release the claim.
type uefiClaim struct {
	claim  *kvsync.Claim
	token  kvsync.Token
	reused bool
	keep   bool
}

// isUEFIBootMode reports whether bootMode boots the guest through UEFI firmware.
func isUEFIBootMode(bootMode string) bool {
	return bootMode == "uefi" || bootMode == "uefi-preferred"
}

// usesUEFIVarStore reports whether v boots through UEFI and so needs a
// variable store. A direct-boot microvm boots qboot and has none.
func usesUEFIVarStore(v *VM) bool {
	return !v.DirectBoot && isUEFIBootMode(v.BootMode)
}

// hasEFIVolume reports whether v still names a <root>-efi volume.
func hasEFIVolume(v *VM) bool {
	v.EBSRequests.Mu.Lock()
	defer v.EBSRequests.Mu.Unlock()
	return slices.ContainsFunc(v.EBSRequests.Requests, func(r EBSRequest) bool { return r.EFI })
}

// CheckUEFIVarStore refuses a UEFI instance that has no variable store, or
// still carries an EFI volume, before anything about it is changed. It only
// reads.
func (m *Manager) CheckUEFIVarStore(ctx context.Context, v *VM) error {
	_, err := m.checkUEFIVarStore(ctx, v)
	return err
}

// checkUEFIVarStore is CheckUEFIVarStore returning the store record it read,
// or nil for an instance that boots without UEFI.
func (m *Manager) checkUEFIVarStore(ctx context.Context, v *VM) (*uefistore.UEFIStoreRecord, error) {
	if !usesUEFIVarStore(v) {
		return nil, nil
	}
	if hasEFIVolume(v) {
		return nil, fmt.Errorf("%w: %s", ErrUEFIEFIVolume, v.ID)
	}
	records := m.deps.UEFIVarStore.Records
	if records == nil {
		return nil, fmt.Errorf("%w: %s, and no store records are wired on this node", ErrUEFIVarStoreMissing, v.ID)
	}
	rec, _, err := records.Get(ctx, v.ID)
	switch {
	case errors.Is(err, kvstore.ErrNotFound):
		return nil, fmt.Errorf("%w: %s has no store record", ErrUEFIVarStoreMissing, v.ID)
	case errors.Is(err, uefistore.ErrInvalidRecord):
		return nil, fmt.Errorf("%w: %w", ErrUEFIVarStoreUnusable, err)
	case err != nil:
		return nil, fmt.Errorf("read UEFI store record for %s: %w: %w", v.ID, err, ErrMountRetryable)
	case rec.State != uefistore.StateLive:
		return nil, fmt.Errorf("%w: %s store record is %s", ErrUEFIVarStoreMissing, v.ID, rec.State)
	case rec.Pointer == nil:
		return nil, fmt.Errorf("%w: %s store record names no pointer", ErrUEFIVarStoreMissing, v.ID)
	}
	return rec, nil
}

// uefiInstanceDir is the directory holding instanceID's vars.fd and
// seeded.json. An ID that is not a single path segment is refused.
func uefiInstanceDir(dataDir, instanceID string) (string, error) {
	if !strings.HasPrefix(instanceID, "i-") || strings.ContainsAny(instanceID, `/\`) || instanceID != filepath.Clean(instanceID) {
		return "", fmt.Errorf("instance ID %q cannot name a UEFI store directory", instanceID)
	}
	return filepath.Join(dataDir, uefiStoreDirName, instanceID), nil
}

// uefiVarsPath is the live variable store QEMU opens as pflash unit 1.
func (m *Manager) uefiVarsPath(instanceID string) (string, error) {
	dir, err := uefiInstanceDir(m.deps.UEFIVarStore.DataDir, instanceID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, uefiVarsFileName), nil
}

// uefiVarsDrive is pflash unit 1 from this node's copy of the store, or nil
// for an instance that boots without UEFI. Launch seeds the copy and a reboot
// reuses it, so a missing one fails rather than booting without it. The drive
// never takes aio=native: inotify cannot see Linux AIO writes.
func (m *Manager) uefiVarsDrive(instance *VM) (*Drive, error) {
	if !usesUEFIVarStore(instance) {
		return nil, nil
	}
	path, err := m.uefiVarsPath(instance.ID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("UEFI variable store for %s: %w", instance.ID, err)
	}
	return &Drive{File: path, Format: "raw", If: "pflash", Unit: 1}, nil
}

// prepareUEFIVarStore checks this node can boot the store rec points at, then
// takes the store's claim, stamps the store record with its generation and
// leaves vars.fd holding the store to boot. rec is the record checkUEFIVarStore
// read. A non-nil claim is returned once one is acquired, failed or not, so
// the caller can release it on a rollback.
func (m *Manager) prepareUEFIVarStore(ctx context.Context, instance *VM, rec *uefistore.UEFIStoreRecord) (*uefiClaim, error) {
	if !usesUEFIVarStore(instance) {
		return nil, nil
	}

	deps := m.deps.UEFIVarStore
	if rec == nil || deps.Objects == nil || deps.Bucket == "" || deps.DataDir == "" || deps.Records == nil || deps.Claims == nil || deps.Liveness == nil {
		return nil, errors.New("UEFI variable store access is not wired on this node")
	}
	if m.deps.InstanceTypes == nil {
		return nil, errors.New("InstanceTypeResolver not wired")
	}
	spec, ok := m.deps.InstanceTypes.Resolve(instance.InstanceType)
	if !ok {
		return nil, fmt.Errorf("instance type %s not found", instance.InstanceType)
	}
	fw, err := NodeFirmwareFor(spec.Architecture)
	if err != nil {
		return nil, err
	}
	dir, err := uefiInstanceDir(deps.DataDir, instance.ID)
	if err != nil {
		return nil, err
	}
	// Before the claim, so a node that cannot boot the store never takes it
	// from a holder that is coming back.
	if err := m.checkUEFIFirmware(instance, rec.Pointer, fw); err != nil {
		return nil, err
	}

	unlock := m.uefi.locks.lock(instance.ID)
	defer unlock()
	held, err := m.acquireUEFIClaim(ctx, instance.ID)
	if err != nil {
		return nil, err
	}
	// Before anything that can fail, so a rollback keeps a claim this node's
	// copy may owe a write-back under.
	local := readLocalUEFICopy(dir)
	held.keep = local.owedUnder(held.token.Generation)
	checked := rec.Pointer.Firmware
	rec, err = deps.Records.Stamp(ctx, instance.ID, held.token.Generation)
	switch {
	case errors.Is(err, uefistore.ErrFenced):
		recordUEFIClaimRefusal("fenced")
		slog.WarnContext(ctx, "UEFI store record is stamped by a later claim than the one this node holds; refusing the start",
			"instanceId", instance.ID, "generation", held.token.Generation, "err", err)
		return held, fmt.Errorf("%w: %w: %w", ErrUEFIClaimHeld, err, ErrMountRetryable)
	case errors.Is(err, kvstore.ErrNotFound), errors.Is(err, uefistore.ErrDeleting):
		return held, fmt.Errorf("%w: %s: %w", ErrUEFIVarStoreMissing, instance.ID, err)
	case errors.Is(err, uefistore.ErrInvalidRecord):
		return held, fmt.Errorf("%w: %w", ErrUEFIVarStoreUnusable, err)
	case err != nil:
		return held, fmt.Errorf("stamp UEFI store record for %s: %w: %w", instance.ID, err, ErrMountRetryable)
	case rec.Pointer == nil:
		return held, fmt.Errorf("%w: %s store record names no pointer", ErrUEFIVarStoreMissing, instance.ID)
	}
	// The record validated its pointer as one object under this instance's prefix.
	ref := rec.Pointer

	if ref.Firmware != checked {
		if err := m.checkUEFIFirmware(instance, ref, fw); err != nil {
			return held, err
		}
	}
	switch {
	case local.varsErr != nil:
		slog.ErrorContext(ctx, "This node's UEFI variable store cannot be checked; keeping it and refusing the start",
			"instanceId", instance.ID, "path", filepath.Join(dir, uefiVarsFileName), "err", local.varsErr)
		return held, fmt.Errorf("UEFI variable store for %s has a %s this node cannot check: %w: %w",
			instance.ID, uefiVarsFileName, local.varsErr, ErrMountRetryable)
	case local.hasVars && local.seedErr != nil:
		slog.ErrorContext(ctx, "This node's UEFI variable store cannot be vouched for; keeping it and refusing the start",
			"instanceId", instance.ID, "path", filepath.Join(dir, uefiSeededFileName), "err", local.seedErr)
		return held, fmt.Errorf("UEFI variable store for %s has a %s this node cannot read: %w: %w",
			instance.ID, uefiSeededFileName, local.seedErr, ErrMountRetryable)
	case held.keep && local.varsSize != fw.Identity.VarsSize:
		slog.ErrorContext(ctx, "This node's UEFI variable store is not the VARS size; keeping it and refusing the start",
			"instanceId", instance.ID, "path", filepath.Join(dir, uefiVarsFileName),
			"size", local.varsSize, "want", fw.Identity.VarsSize)
		return held, fmt.Errorf("%w: %s for %s is %d bytes, not %d",
			ErrUEFIVarStoreUnusable, uefiVarsFileName, instance.ID, local.varsSize, fw.Identity.VarsSize)
	case held.keep:
		held.reused = true
		recordUEFIStoreSeed("local")
		slog.InfoContext(ctx, "Reusing this node's UEFI variable store",
			"instanceId", instance.ID, "key", ref.Key, "seededKey", local.seed.Key, "generation", held.token.Generation)
		sum := ""
		if local.seed.Key == ref.Key {
			sum = local.seed.Sum
		}
		m.registerUEFIWriter(instance.ID, dir, held.token, ref.Key, sum, fw)
		return held, nil
	}

	start := time.Now()
	region, err := fetchUEFIVars(ctx, deps, ref.Key, fw.Identity.VarsSize)
	if err != nil {
		return held, err
	}
	if err := writeFileAtomic(dir, uefiVarsFileName, region); err != nil {
		return held, fmt.Errorf("write UEFI variable store for %s: %w", instance.ID, err)
	}
	sum := uefiRegionSum(region)
	if err := writeUEFISeed(dir, uefiSeed{
		Key: ref.Key, Generation: held.token.Generation, SeededAt: time.Now().UTC(),
		Firmware: fw.Identity, CodePath: fw.Code, Sum: sum,
	}); err != nil {
		return held, fmt.Errorf("write %s for %s: %w", uefiSeededFileName, instance.ID, err)
	}
	recordUEFIStoreSeed("object")
	slog.InfoContext(ctx, "Seeded UEFI variable store from the object store",
		"instanceId", instance.ID, "key", ref.Key, "generation", held.token.Generation,
		"bytes", len(region), "fetch_ms", otelsetup.Millis(time.Since(start)))
	m.registerUEFIWriter(instance.ID, dir, held.token, ref.Key, sum, fw)
	return held, nil
}

// acquireUEFIClaim takes instanceID's store claim, or adopts it when this node
// already holds it. A claim another node holds is taken over only once that
// node's heartbeat is LivenessWindow old or absent; while it is fresh, or
// cannot be read, the start is refused as retryable (ADR-0003:S7).
func (m *Manager) acquireUEFIClaim(ctx context.Context, instanceID string) (*uefiClaim, error) {
	deps := m.deps.UEFIVarStore
	claim := deps.Claims.Get(instanceID)
	tok, err := claim.TryLock(ctx)
	if err == nil {
		return &uefiClaim{claim: claim, token: tok}, nil
	}
	he, ok := errors.AsType[*kvsync.HeldError](err)
	if !ok || he.Current.Holder == "" {
		return nil, fmt.Errorf("take UEFI store claim for %s: %w: %w", instanceID, err, ErrMountRetryable)
	}
	holder := he.Current

	seen, found, err := deps.Liveness.LastSeen(ctx, holder.Holder)
	if err != nil {
		recordUEFIClaimRefusal("liveness_unknown")
		slog.WarnContext(ctx, "Refusing to start: another node holds the UEFI variable store claim and its heartbeat cannot be read",
			"instanceId", instanceID, "holder", holder.Holder, "generation", holder.Generation, "err", err)
		return nil, fmt.Errorf("%w: %s holds %s and its liveness could not be read: %w: %w",
			ErrUEFIClaimHeld, holder.Holder, instanceID, err, ErrMountRetryable)
	}
	now := time.Now()
	if found && clusterv1.LiveAt(seen, now) {
		recordUEFIClaimRefusal("holder_live")
		slog.WarnContext(ctx, "Refusing to start: another live node holds the UEFI variable store claim",
			"instanceId", instanceID, "holder", holder.Holder, "generation", holder.Generation,
			"heartbeat_age_ms", otelsetup.Millis(now.Sub(seen)))
		return nil, fmt.Errorf("%w: %s holds %s and is still heartbeating: %w",
			ErrUEFIClaimHeld, holder.Holder, instanceID, ErrMountRetryable)
	}

	tok, err = claim.TakeOver(ctx, holder)
	if err != nil {
		if _, moved := errors.AsType[*kvsync.HeldError](err); moved {
			recordUEFIClaimRefusal("moved")
			return nil, fmt.Errorf("%w: the claim on %s moved on from %s: %w: %w",
				ErrUEFIClaimHeld, instanceID, holder.Holder, err, ErrMountRetryable)
		}
		return nil, fmt.Errorf("take over UEFI store claim for %s: %w: %w", instanceID, err, ErrMountRetryable)
	}
	recordUEFIClaimTakeover()
	attrs := []any{"instanceId", instanceID, "holder", holder.Holder, "fromGeneration", holder.Generation, "generation", tok.Generation}
	if found {
		attrs = append(attrs, "heartbeat_age_ms", otelsetup.Millis(now.Sub(seen)))
	}
	slog.WarnContext(ctx, "Took over the UEFI variable store claim from a node whose heartbeat has stopped; variable changes it never wrote back are lost", attrs...)
	return &uefiClaim{claim: claim, token: tok}, nil
}

// releaseUEFIClaim unlocks the claim a launch that rolled back before the
// guest ran acquired. A launch holding this node's copy keeps it: the copy
// may hold changes no object has, and another node would start without them.
func (m *Manager) releaseUEFIClaim(ctx context.Context, instanceID string, held *uefiClaim) {
	if held == nil || held.keep {
		return
	}
	m.unlockUEFIClaim(ctx, instanceID, held.token, "the launch rolled back before the guest ran")
}

// unlockUEFIClaim releases instanceID's claim while it still holds tok. A
// failed unlock leaves the claim with this node, which only delays a start
// elsewhere, so it is logged and not returned.
func (m *Manager) unlockUEFIClaim(ctx context.Context, instanceID string, tok kvsync.Token, why string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uefiClaimReleaseTimeout)
	defer cancel()
	if err := m.deps.UEFIVarStore.Claims.Get(instanceID).Unlock(ctx, tok); err != nil {
		slog.WarnContext(ctx, "Failed to release the UEFI variable store claim",
			"instanceId", instanceID, "generation", tok.Generation, "why", why, "err", err)
		return false
	}
	slog.InfoContext(ctx, "Released the UEFI variable store claim", "instanceId", instanceID, "generation", tok.Generation, "why", why)
	return true
}

// releaseHeldUEFIClaim releases instanceID's claim when this node holds it, at
// whatever generation, for a terminate that has recorded the store's deletion.
func (m *Manager) releaseHeldUEFIClaim(ctx context.Context, instanceID, why string) {
	claims := m.deps.UEFIVarStore.Claims
	if claims == nil {
		return
	}
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), uefiClaimTimeout)
	holder, held, err := claims.Get(instanceID).Holder(readCtx)
	cancel()
	switch {
	case err != nil:
		slog.WarnContext(ctx, "Failed to read the UEFI variable store claim; the store reaper removes it",
			"instanceId", instanceID, "err", err)
	case held && holder.Holder == m.deps.NodeID:
		m.unlockUEFIClaim(ctx, instanceID, holder, why)
	}
}

// checkUEFIFirmware fails a store whose arch or VARS size differs from this
// node's, naming both firmwares in the log and the StateReason. A different
// CODE build boots, because a package update changes it on every node.
func (m *Manager) checkUEFIFirmware(instance *VM, ref *uefistore.UEFIVarStoreRef, fw NodeFirmware) error {
	stored, node := ref.Firmware, fw.Identity
	var kind string
	switch {
	case stored.Arch != node.Arch:
		kind = "arch"
	case stored.VarsSize != node.VarsSize:
		kind = "vars_size"
	case stored.CodeSHA256 != node.CodeSHA256:
		recordUEFIFirmwareMismatch("code")
		slog.Warn("UEFI variable store was written under a different CODE build; booting it",
			"instanceId", instance.ID, "key", ref.Key, "writerNode", ref.WriterNode,
			"storeCodeSha256", stored.CodeSHA256, "nodeCodeSha256", node.CodeSHA256, "nodeCodePath", fw.Code)
		return nil
	default:
		return nil
	}

	recordUEFIFirmwareMismatch(kind)
	msg := fmt.Sprintf("%s: the UEFI variable store %s was written under firmware arch=%s vars_size=%d code_sha256=%s, and this node runs arch=%s vars_size=%d code_sha256=%s (%s)",
		uefiStateReasonCode, ref.Key, stored.Arch, stored.VarsSize, stored.CodeSHA256,
		node.Arch, node.VarsSize, node.CodeSHA256, fw.Code)
	m.Inspect(instance, func(v *VM) {
		if v.Instance != nil {
			v.Instance.StateReason = &ec2.StateReason{Code: aws.String(uefiStateReasonCode), Message: aws.String(msg)}
		}
	})
	slog.Error("UEFI variable store firmware does not match this node; refusing the launch",
		"instanceId", instance.ID, "key", ref.Key, "writerNode", ref.WriterNode, "mismatch", kind,
		"storeArch", stored.Arch, "storeVarsSize", stored.VarsSize, "storeCodeSha256", stored.CodeSHA256,
		"nodeArch", node.Arch, "nodeVarsSize", node.VarsSize, "nodeCodeSha256", node.CodeSHA256, "nodeCodePath", fw.Code)
	return fmt.Errorf("%w: %s", ErrUEFIFirmwareMismatch, msg)
}

// localUEFICopy is what this node holds for an instance: whether vars.fd
// exists and its size, and seeded.json or the error reading it.
type localUEFICopy struct {
	hasVars  bool
	varsSize int64
	varsErr  error
	hasSeed  bool
	seed     uefiSeed
	seedErr  error
}

// readLocalUEFICopy reads dir's vars.fd and seeded.json. A vars.fd that may
// exist but cannot be checked sets varsErr and counts as present; only a
// seeded.json that exists and cannot be read or decoded sets seedErr.
func readLocalUEFICopy(dir string) localUEFICopy {
	var c localUEFICopy
	switch info, err := os.Stat(filepath.Join(dir, uefiVarsFileName)); {
	case err == nil:
		c.hasVars, c.varsSize = true, info.Size()
	case !errors.Is(err, fs.ErrNotExist):
		c.hasVars, c.varsErr = true, err
	}
	data, err := os.ReadFile(filepath.Join(dir, uefiSeededFileName))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return c
	case err != nil:
		c.seedErr = err
	default:
		c.seedErr = json.Unmarshal(data, &c.seed)
	}
	c.hasSeed = c.seedErr == nil
	return c
}

// owedUnder reports whether the copy may hold changes no object has under the
// claim generation: vars.fd exists and seeded.json names generation, or cannot
// be read. At one generation only this node moves the pointer, and only from
// vars.fd, so such a file is never older than the pointer whatever key it names.
func (c localUEFICopy) owedUnder(generation uint64) bool {
	if !c.hasVars {
		return false
	}
	return c.varsErr != nil || c.seedErr != nil || (c.hasSeed && c.seed.Generation == generation)
}

// fetchUEFIVars gets key and returns its decompressed region, which must be
// exactly varsSize bytes. A failed get is retryable; a missing or malformed
// object is not.
func fetchUEFIVars(ctx context.Context, deps UEFIVarStoreDeps, key string, varsSize int64) ([]byte, error) {
	getCtx, cancel := context.WithTimeout(ctx, uefiStoreGetTimeout)
	defer cancel()

	out, err := deps.Objects.GetObject(getCtx, &s3.GetObjectInput{Bucket: aws.String(deps.Bucket), Key: aws.String(key)})
	if err != nil {
		if objectstore.IsNoSuchKeyError(err) {
			return nil, fmt.Errorf("%w: %s does not exist", ErrUEFIVarStoreUnusable, key)
		}
		return nil, fmt.Errorf("get UEFI variable store %s: %w: %w", key, err, ErrMountRetryable)
	}
	defer out.Body.Close()

	limit := varsSize + uefiStoreBodySlack
	raw, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read UEFI variable store %s: %w: %w", key, err, ErrMountRetryable)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: %s is over %d bytes compressed", ErrUEFIVarStoreUnusable, key, limit)
	}

	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not gzip: %w", ErrUEFIVarStoreUnusable, key, err)
	}
	region, err := io.ReadAll(io.LimitReader(gz, varsSize+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %s does not decompress: %w", ErrUEFIVarStoreUnusable, key, err)
	}
	if int64(len(region)) != varsSize {
		return nil, fmt.Errorf("%w: %s holds a %d-byte region, and this node's VARS template is %d bytes",
			ErrUEFIVarStoreUnusable, key, len(region), varsSize)
	}
	return region, nil
}

// readUEFISeed reads dir's seeded.json.
func readUEFISeed(dir string) (uefiSeed, error) {
	var seed uefiSeed
	data, err := os.ReadFile(filepath.Join(dir, uefiSeededFileName))
	if err != nil {
		return seed, err
	}
	if err := json.Unmarshal(data, &seed); err != nil {
		return seed, fmt.Errorf("decode %s: %w", filepath.Join(dir, uefiSeededFileName), err)
	}
	return seed, nil
}

// writeUEFISeed replaces dir's seeded.json with seed.
func writeUEFISeed(dir string, seed uefiSeed) error {
	data, err := json.Marshal(seed)
	if err != nil {
		return err
	}
	return writeFileAtomic(dir, uefiSeededFileName, data)
}

// writeFileAtomic replaces dir/name with data by temporary file and rename,
// syncing both, so a crash leaves the old file or the new one.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// uefiStoreMeterName is the instrumentation scope for variable store launches.
const uefiStoreMeterName = "github.com/mulgadc/spinifex/spinifex/runtime/compute/vm/uefistore"

var (
	uefiStoreMetricsOnce   sync.Once
	uefiStoreSeedCounter   metric.Int64Counter
	uefiFirmwareMismatches metric.Int64Counter
	uefiClaimRefusals      metric.Int64Counter
	uefiClaimTakeovers     metric.Int64Counter
)

func uefiStoreMetrics() {
	uefiStoreMetricsOnce.Do(func() {
		meter := otel.Meter(uefiStoreMeterName)
		var err error
		uefiStoreSeedCounter, err = meter.Int64Counter("mulga.vm.uefi_store.seeds",
			metric.WithDescription("UEFI launches by where their variable store came from: this node's copy or the object store."),
			metric.WithUnit("{launch}"))
		if err != nil {
			otel.Handle(err)
		}
		uefiFirmwareMismatches, err = meter.Int64Counter("mulga.vm.uefi_store.firmware_mismatches",
			metric.WithDescription("UEFI launches whose variable store was written under different firmware, by what differed. Only kind=code boots."),
			metric.WithUnit("{launch}"))
		if err != nil {
			otel.Handle(err)
		}
		uefiClaimRefusals, err = meter.Int64Counter("mulga.vm.uefi_store.claim_refusals",
			metric.WithDescription("UEFI starts refused because another node holds the variable store claim and may be live, by reason."),
			metric.WithUnit("{start}"))
		if err != nil {
			otel.Handle(err)
		}
		uefiClaimTakeovers, err = meter.Int64Counter("mulga.vm.uefi_store.claim_takeovers",
			metric.WithDescription("UEFI variable store claims taken over from a node whose heartbeat had stopped."),
			metric.WithUnit("{start}"))
		if err != nil {
			otel.Handle(err)
		}
	})
}

func recordUEFIStoreSeed(source string) {
	uefiStoreMetrics()
	if uefiStoreSeedCounter != nil {
		uefiStoreSeedCounter.Add(context.Background(), 1, metric.WithAttributes(attribute.String("source", source)))
	}
}

func recordUEFIFirmwareMismatch(kind string) {
	uefiStoreMetrics()
	if uefiFirmwareMismatches != nil {
		uefiFirmwareMismatches.Add(context.Background(), 1, metric.WithAttributes(attribute.String("kind", kind)))
	}
}

func recordUEFIClaimRefusal(reason string) {
	uefiStoreMetrics()
	if uefiClaimRefusals != nil {
		uefiClaimRefusals.Add(context.Background(), 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

func recordUEFIClaimTakeover() {
	uefiStoreMetrics()
	if uefiClaimTakeovers != nil {
		uefiClaimTakeovers.Add(context.Background(), 1)
	}
}
