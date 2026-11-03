package vm

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
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

	// uefiStoreBodySlack is how far a compressed body may exceed its region
	// before it is refused; gzip grows incompressible data by far less.
	uefiStoreBodySlack = 1 << 20

	// uefiStateReasonCode is the StateReason a launch refused over its
	// variable store records, as EC2 reports an internal launch failure.
	uefiStateReasonCode = "Server.InternalError"
)

// ErrUEFIVarStoreMissing is a UEFI instance whose record names no variable
// store, so it has nothing to boot its variables from.
var ErrUEFIVarStoreMissing = errors.New("UEFI instance has no variable store")

// ErrUEFIFirmwareMismatch is a variable store written under firmware whose
// architecture or VARS size differs from this node's, which pflash cannot load.
var ErrUEFIFirmwareMismatch = errors.New("UEFI variable store firmware does not match this node")

// ErrUEFIVarStoreUnusable is a variable store object that is missing or does
// not decode to a region of this node's VARS size. Retrying does not help.
var ErrUEFIVarStoreUnusable = errors.New("UEFI variable store object is unusable")

// UEFIVarStoreDeps is where a node reads instance variable stores from and
// keeps the live copy QEMU opens. DataDir is the daemon's persistent data
// directory, so the copy survives a daemon restart and a host reboot.
type UEFIVarStoreDeps struct {
	Objects objectstore.ObjectStore
	Bucket  string
	DataDir string
	// Records and Instances carry write-back obligations and move the
	// pointer; write-back is off on a node without them.
	Records   UEFIStoreRecords
	Instances UEFIInstancePointers
}

// uefiSeed is seeded.json: the pointer key vars.fd beside it was seeded from.
type uefiSeed struct {
	Key      string    `json:"key"`
	SeededAt time.Time `json:"seeded_at"`
}

// isUEFIBootMode reports whether bootMode boots the guest through UEFI firmware.
func isUEFIBootMode(bootMode string) bool {
	return bootMode == "uefi" || bootMode == "uefi-preferred"
}

// UsesUEFIVarStore reports whether v boots through UEFI and so needs a
// variable store. A direct-boot microvm boots qboot and has none.
func UsesUEFIVarStore(v *VM) bool {
	return !v.DirectBoot && isUEFIBootMode(v.BootMode)
}

// CheckUEFIVarStore refuses a UEFI instance whose record names no variable
// store, before anything about it is changed. The caller owns v.
func CheckUEFIVarStore(v *VM) error {
	if UsesUEFIVarStore(v) && v.UEFIVarStore == nil {
		return fmt.Errorf("%w: %s", ErrUEFIVarStoreMissing, v.ID)
	}
	return nil
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

// uefiVarsDrive is pflash unit 1 from this node's copy of the store the
// record points at, or nil for a guest that boots without UEFI. Launch seeds
// the copy and a reboot reuses it, so a missing one fails the boot.
func (m *Manager) uefiVarsDrive(instance *VM) (*Drive, error) {
	if !UsesUEFIVarStore(instance) {
		return nil, nil
	}
	hasPointer := false
	m.Inspect(instance, func(v *VM) { hasPointer = v.UEFIVarStore != nil })
	if !hasPointer {
		return nil, fmt.Errorf("%w: %s", ErrUEFIVarStoreMissing, instance.ID)
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

// prepareUEFIVarStore leaves vars.fd holding the store the record points at,
// reusing this node's copy when seeded.json names the pointer and fetching
// the object otherwise. The object and the record's pointer are only read.
func (m *Manager) prepareUEFIVarStore(ctx context.Context, instance *VM) error {
	var ref *UEFIVarStoreRef
	m.Inspect(instance, func(v *VM) {
		if v.UEFIVarStore != nil {
			cp := *v.UEFIVarStore
			ref = &cp
		}
	})
	if ref == nil || !UsesUEFIVarStore(instance) {
		return nil
	}

	deps := m.deps.UEFIVarStore
	if deps.Objects == nil || deps.Bucket == "" || deps.DataDir == "" {
		return errors.New("UEFI variable store access is not wired on this node")
	}
	if name, ok := strings.CutPrefix(ref.Key, uefistore.KeyPrefix(instance.ID)); !ok || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("%w: pointer %q is not under %s", ErrUEFIVarStoreUnusable, ref.Key, uefistore.KeyPrefix(instance.ID))
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
	if err := m.checkUEFIFirmware(instance, ref, fw); err != nil {
		return err
	}

	dir, err := uefiInstanceDir(deps.DataDir, instance.ID)
	if err != nil {
		return err
	}
	if reuseUEFIVars(dir, ref.Key, fw.Identity.VarsSize) {
		recordUEFIStoreSeed("local")
		slog.InfoContext(ctx, "Reusing this node's UEFI variable store", "instanceId", instance.ID, "key", ref.Key)
		return nil
	}

	start := time.Now()
	region, err := fetchUEFIVars(ctx, deps, ref.Key, fw.Identity.VarsSize)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(dir, uefiVarsFileName, region); err != nil {
		return fmt.Errorf("write UEFI variable store for %s: %w", instance.ID, err)
	}
	if err := writeUEFISeed(dir, ref.Key); err != nil {
		return fmt.Errorf("write %s for %s: %w", uefiSeededFileName, instance.ID, err)
	}
	m.clearForeignUEFIWriteBack(ctx, instance.ID)
	recordUEFIStoreSeed("object")
	slog.InfoContext(ctx, "Seeded UEFI variable store from the object store",
		"instanceId", instance.ID, "key", ref.Key, "bytes", len(region), "fetch_ms", otelsetup.Millis(time.Since(start)))
	return nil
}

// checkUEFIFirmware fails a store whose arch or VARS size differs from this
// node's, naming both firmwares in the log and the StateReason. A different
// CODE build boots, because a package update changes it on every node.
func (m *Manager) checkUEFIFirmware(instance *VM, ref *UEFIVarStoreRef, fw NodeFirmware) error {
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

// reuseUEFIVars reports whether dir already holds the store key names: a
// seeded.json naming key beside a vars.fd of the region's size.
func reuseUEFIVars(dir, key string, varsSize int64) bool {
	data, err := os.ReadFile(filepath.Join(dir, uefiSeededFileName))
	if err != nil {
		return false
	}
	var seed uefiSeed
	if err := json.Unmarshal(data, &seed); err != nil || seed.Key != key {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, uefiVarsFileName))
	if err != nil || info.Size() != varsSize {
		slog.Warn("UEFI variable store seeded here has no usable vars.fd; fetching it again", "dir", dir, "key", key)
		return false
	}
	return true
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
	uefiWriteBacks         metric.Int64Counter
	uefiWriteBackSkips     metric.Int64Counter
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
		uefiWriteBacks, err = meter.Int64Counter("mulga.vm.uefi_store.write_backs",
			metric.WithDescription("UEFI variable store write-backs by outcome: written, deferred to the reaper, retried by it, dropped, or failed."),
			metric.WithUnit("{write_back}"))
		if err != nil {
			otel.Handle(err)
		}
		uefiWriteBackSkips, err = meter.Int64Counter("mulga.vm.uefi_store.write_back_skips",
			metric.WithDescription("Sealing stops that wrote no UEFI variable store back, by the check that refused it."),
			metric.WithUnit("{stop}"))
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
