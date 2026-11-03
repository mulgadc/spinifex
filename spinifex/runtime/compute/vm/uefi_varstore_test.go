package vm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	testUEFIBucket   = "instances"
	testUEFIVarsSize = 4096
	testUEFIInstance = "i-0123456789abcdef0"
)

var (
	uefiMetricsReaderOnce sync.Once
	uefiMetricsReader     *sdkmetric.ManualReader
)

// uefiCounter returns the sum of the named counter's points whose attribute
// key has value.
func uefiCounter(t *testing.T, name, key, value string) int64 {
	t.Helper()
	uefiMetricsReaderOnce.Do(func() {
		uefiMetricsReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(uefiMetricsReader)))
	})
	var rm metricdata.ResourceMetrics
	require.NoError(t, uefiMetricsReader.Collect(context.Background(), &rm))
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if m.Name != name || !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == value {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// uefiFixture is a node with one x86_64 firmware candidate, an in-memory
// object store and a data directory.
type uefiFixture struct {
	m        *Manager
	mounter  *fakeVolumeMounter
	objects  *objectstore.MemoryObjectStore
	dataDir  string
	codePath string
	identity uefistore.FirmwareIdentity
}

func newUEFIFixture(t *testing.T) *uefiFixture {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	fwDir := t.TempDir()
	code := filepath.Join(fwDir, "OVMF_CODE.fd")
	vars := filepath.Join(fwDir, "OVMF_VARS.fd")
	codeBytes := []byte("ovmf code build 1")
	require.NoError(t, os.WriteFile(code, codeBytes, 0o600))
	require.NoError(t, os.WriteFile(vars, make([]byte, testUEFIVarsSize), 0o600))
	orig := FirmwarePathCandidates
	FirmwarePathCandidates = map[string][]FirmwareCandidate{"x86_64": {{Code: code, VarsTemplate: vars}}}
	t.Cleanup(func() { FirmwarePathCandidates = orig })

	sum := sha256.Sum256(codeBytes)
	f := &uefiFixture{
		mounter:  &fakeVolumeMounter{},
		objects:  objectstore.NewMemoryObjectStore(),
		dataDir:  t.TempDir(),
		codePath: code,
		identity: uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: testUEFIVarsSize, CodeSHA256: hex.EncodeToString(sum[:])},
	}
	f.m = NewManagerWithDeps(Deps{
		VolumeMounter: f.mounter,
		InstanceTypes: fakeInstanceTypeResolver{"t3.nano": {Architecture: "x86_64", VCPUs: 1, MemoryMiB: 512}},
		UEFIVarStore:  UEFIVarStoreDeps{Objects: f.objects, Bucket: testUEFIBucket, DataDir: f.dataDir},
	})
	return f
}

// region is a VARS region of the fixture's size whose bytes say which store it is.
func region(tag string) []byte {
	b := make([]byte, testUEFIVarsSize)
	copy(b, tag)
	return b
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func (f *uefiFixture) putObject(t *testing.T, key string, body []byte) {
	t.Helper()
	_, err := f.objects.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(testUEFIBucket), Key: aws.String(key), Body: bytes.NewReader(body),
	})
	require.NoError(t, err)
}

func (f *uefiFixture) objectBody(t *testing.T, key string) []byte {
	t.Helper()
	out, err := f.objects.GetObject(context.Background(), &s3.GetObjectInput{Bucket: aws.String(testUEFIBucket), Key: aws.String(key)})
	require.NoError(t, err)
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	require.NoError(t, err)
	return b
}

func (f *uefiFixture) instanceDir() string {
	return filepath.Join(f.dataDir, "uefi", testUEFIInstance)
}

func (f *uefiFixture) writeLocal(t *testing.T, seededKey string, vars []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(f.instanceDir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), "vars.fd"), vars, 0o600))
	seed, err := json.Marshal(uefiSeed{Key: seededKey, SeededAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), "seeded.json"), seed, 0o600))
}

func (f *uefiFixture) localVars(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.instanceDir(), "vars.fd"))
	require.NoError(t, err)
	return b
}

func (f *uefiFixture) seededKey(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.instanceDir(), "seeded.json"))
	require.NoError(t, err)
	var seed uefiSeed
	require.NoError(t, json.Unmarshal(b, &seed))
	return seed.Key
}

func (f *uefiFixture) instance(key string, fw uefistore.FirmwareIdentity) *VM {
	v := &VM{
		ID: testUEFIInstance, InstanceType: "t3.nano", BootMode: "uefi", Status: StateStopped,
		Instance: &ec2.Instance{},
	}
	if key != "" {
		v.UEFIVarStore = &UEFIVarStoreRef{Key: key, Firmware: fw, WriterNode: "node-a", WrittenAt: time.Unix(1700000000, 0).UTC()}
	}
	f.m.Insert(v)
	return v
}

func keyFor(name string) string { return uefistore.KeyPrefix(testUEFIInstance) + name }

func TestPrepareUEFIVarStore_FetchesTheObjectAndRecordsTheSeed(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("11111111-1111-1111-1111-111111111111")
	f.putObject(t, key, gzipped(t, region("converted")))
	instance := f.instance(key, f.identity)
	before := uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "object")

	require.NoError(t, f.m.prepareUEFIVarStore(t.Context(), instance))

	assert.Equal(t, region("converted"), f.localVars(t), "vars.fd must hold the decompressed region")
	assert.Equal(t, key, f.seededKey(t))
	assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "object"))
	assert.Equal(t, key, instance.UEFIVarStore.Key, "seeding only reads the pointer")
}

// A start on the node that seeded the pointer boots that node's copy, which
// can be newer than the object, and makes no get at all.
func TestPrepareUEFIVarStore_ReusesTheLocalCopyWhenSeededNamesThePointer(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("22222222-2222-2222-2222-222222222222")
	f.writeLocal(t, key, region("guest wrote this"))
	instance := f.instance(key, f.identity)
	before := uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "local")

	require.NoError(t, f.m.prepareUEFIVarStore(t.Context(), instance), "no object exists, so a get would fail")

	assert.Equal(t, region("guest wrote this"), f.localVars(t))
	assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "local"))
}

// Once the pointer has moved past what this node seeded, the local copy is
// stale and the pointed object replaces it.
func TestPrepareUEFIVarStore_RefetchesWhenTheLocalCopyIsNotThePointer(t *testing.T) {
	tests := []struct {
		name  string
		local func(f *uefiFixture, t *testing.T)
	}{
		{name: "seeded from an older pointer", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("older"), region("stale"))
		}},
		{name: "vars.fd of the wrong size", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("current"), []byte("short"))
		}},
		{name: "seeded.json unreadable", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("current"), region("stale"))
			require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), "seeded.json"), []byte("{"), 0o600))
		}},
		{name: "no vars.fd", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("current"), region("stale"))
			require.NoError(t, os.Remove(filepath.Join(f.instanceDir(), "vars.fd")))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			key := keyFor("current")
			tt.local(f, t)
			f.putObject(t, key, gzipped(t, region("restored")))
			instance := f.instance(key, f.identity)

			require.NoError(t, f.m.prepareUEFIVarStore(t.Context(), instance))

			assert.Equal(t, region("restored"), f.localVars(t))
			assert.Equal(t, key, f.seededKey(t))
		})
	}
}

// A store pflash cannot load fails the launch before any volume is mounted,
// names both firmwares, and leaves the object, the pointer and the local
// copy as they were.
func TestRun_UEFIFirmwareMismatchFailsTheLaunchAndChangesNothing(t *testing.T) {
	tests := []struct {
		name   string
		kind   string
		mutate func(*uefistore.FirmwareIdentity)
		want   []string
	}{
		{name: "arch", kind: "arch", mutate: func(fw *uefistore.FirmwareIdentity) { fw.Arch = "arm64" },
			want: []string{"arch=arm64", "arch=x86_64"}},
		{name: "vars size", kind: "vars_size", mutate: func(fw *uefistore.FirmwareIdentity) { fw.VarsSize = 67108864 },
			want: []string{"vars_size=67108864", "vars_size=4096"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			key := keyFor("mismatch")
			body := gzipped(t, region("other firmware"))
			f.putObject(t, key, body)
			f.writeLocal(t, keyFor("older"), region("local"))
			stored := f.identity
			tt.mutate(&stored)
			instance := f.instance(key, stored)
			wantRef := *instance.UEFIVarStore
			before := uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", tt.kind)

			err := f.m.Run(t.Context(), instance)

			require.ErrorIs(t, err, ErrUEFIFirmwareMismatch)
			require.NotErrorIs(t, err, ErrMountRetryable, "a mismatch does not clear with time")
			assert.Empty(t, f.mounter.mounted, "nothing is mounted for a store that cannot boot")
			require.NotNil(t, instance.Instance.StateReason)
			assert.Equal(t, "Server.InternalError", aws.StringValue(instance.Instance.StateReason.Code))
			for _, s := range append(tt.want, f.identity.CodeSHA256, f.codePath, key) {
				assert.Contains(t, aws.StringValue(instance.Instance.StateReason.Message), s)
			}
			assert.Equal(t, wantRef, *instance.UEFIVarStore, "the pointer is unchanged")
			assert.Equal(t, body, f.objectBody(t, key), "the object is unchanged")
			assert.Equal(t, region("local"), f.localVars(t), "the local copy is unchanged")
			assert.Equal(t, keyFor("older"), f.seededKey(t))
			assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", tt.kind))
		})
	}
}

// An ovmf package update changes the CODE hash on every node, so a store
// written under another build boots and the difference is counted.
func TestPrepareUEFIVarStore_DifferentCodeBuildBootsAndIsCounted(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("code")
	f.putObject(t, key, gzipped(t, region("older ovmf")))
	stored := f.identity
	stored.CodeSHA256 = strings.Repeat("0", 64)
	instance := f.instance(key, stored)
	before := uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", "code")

	require.NoError(t, f.m.prepareUEFIVarStore(t.Context(), instance))

	assert.Equal(t, region("older ovmf"), f.localVars(t))
	assert.Nil(t, instance.Instance.StateReason)
	assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", "code"))
}

// failingGetStore fails every get, standing in for an object store that
// does not answer.
type failingGetStore struct {
	*objectstore.MemoryObjectStore
	err error
}

func (s *failingGetStore) GetObject(context.Context, *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
	return nil, s.err
}

func TestPrepareUEFIVarStore_FailedGetIsRetryable(t *testing.T) {
	f := newUEFIFixture(t)
	f.m.deps.UEFIVarStore.Objects = &failingGetStore{MemoryObjectStore: f.objects, err: errors.New("connection refused")}
	instance := f.instance(keyFor("down"), f.identity)

	err := f.m.prepareUEFIVarStore(t.Context(), instance)

	require.ErrorIs(t, err, ErrMountRetryable, "restore backs off on a get as it does on a mount")
	assert.NoFileExists(t, filepath.Join(f.instanceDir(), "seeded.json"))
}

// An object that is missing or does not decode to this node's region is
// refused for good, and nothing is written locally.
func TestPrepareUEFIVarStore_UnusableObjectIsRefused(t *testing.T) {
	tests := []struct {
		name string
		body []byte
	}{
		{name: "missing"},
		{name: "not gzip", body: region("raw, uncompressed")},
		{name: "short region", body: gzipped(t, make([]byte, testUEFIVarsSize-1))},
		{name: "long region", body: gzipped(t, make([]byte, testUEFIVarsSize+1))},
		{name: "truncated gzip", body: gzipped(t, region("cut"))[:20]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			key := keyFor("bad")
			if tt.body != nil {
				f.putObject(t, key, tt.body)
			}
			instance := f.instance(key, f.identity)

			err := f.m.prepareUEFIVarStore(t.Context(), instance)

			require.ErrorIs(t, err, ErrUEFIVarStoreUnusable)
			require.NotErrorIs(t, err, ErrMountRetryable)
			assert.NoFileExists(t, filepath.Join(f.instanceDir(), "vars.fd"))
			assert.NoFileExists(t, filepath.Join(f.instanceDir(), "seeded.json"))
		})
	}
}

func TestPrepareUEFIVarStore_PointerOutsideTheInstancePrefixIsRefused(t *testing.T) {
	for _, key := range []string{"uefi/i-0aaaaaaaaaaaaaaaa/other", keyFor("a/b"), uefistore.KeyPrefix(testUEFIInstance)} {
		t.Run(key, func(t *testing.T) {
			f := newUEFIFixture(t)
			f.putObject(t, key, gzipped(t, region("x")))
			instance := f.instance(key, f.identity)

			require.ErrorIs(t, f.m.prepareUEFIVarStore(t.Context(), instance), ErrUEFIVarStoreUnusable)
		})
	}
}

// A BIOS instance, a microvm and a legacy instance with no pointer touch
// neither the object store nor the data directory.
func TestPrepareUEFIVarStore_NoPointerIsANoOp(t *testing.T) {
	f := newUEFIFixture(t)
	f.m.deps.UEFIVarStore.Objects = &failingGetStore{MemoryObjectStore: f.objects, err: errors.New("must not be called")}
	bios := f.instance(keyFor("bios"), f.identity)
	bios.BootMode = ""
	require.NoError(t, f.m.prepareUEFIVarStore(t.Context(), bios))

	legacy := f.instance("", f.identity)
	require.NoError(t, f.m.prepareUEFIVarStore(t.Context(), legacy))
	assert.NoDirExists(t, filepath.Join(f.dataDir, "uefi"))
}

// A UEFI instance with no pointer, or two stores, is refused before the
// launch clears or changes anything on its record.
func TestRun_UEFIInstanceWithoutExactlyOneStoreIsRefused(t *testing.T) {
	efiVolume := EBSRequest{Name: "vol-1-efi", EFI: true}
	tests := []struct {
		name    string
		pointer bool
		efi     bool
		want    error
	}{
		{name: "no pointer and no EFI volume", want: ErrUEFIVarStoreMissing},
		{name: "a pointer and an EFI volume", pointer: true, efi: true, want: ErrUEFIVarStoreConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			key := ""
			if tt.pointer {
				key = keyFor("p")
			}
			instance := f.instance(key, f.identity)
			if tt.efi {
				instance.EBSRequests.Requests = []EBSRequest{efiVolume}
			}
			stale := &ec2.StateReason{Code: aws.String("Client.UserInitiatedShutdown"), Message: aws.String("Client.UserInitiatedShutdown: User initiated shutdown")}
			instance.Instance.StateReason = stale
			before := instance.Record()

			err := f.m.Run(t.Context(), instance)

			require.ErrorIs(t, err, tt.want)
			assert.Empty(t, f.mounter.mounted)
			assert.Equal(t, before, instance.Record(), "the record is unchanged")
		})
	}
}

// Start from StateError transitions to pending before launching; a refused
// UEFI instance must not take that transition either.
func TestStart_UEFIInstanceWithoutAPointerIsRefusedBeforeTheTransition(t *testing.T) {
	f := newUEFIFixture(t)
	transitions := 0
	f.m.deps.TransitionState = func(v *VM, s InstanceState) error { transitions++; v.Status = s; return nil }
	instance := f.instance("", f.identity)
	instance.Status = StateError

	err := f.m.Start(t.Context(), instance.ID)

	require.ErrorIs(t, err, ErrUEFIVarStoreMissing)
	assert.Equal(t, 0, transitions)
	assert.Equal(t, StateError, instance.Status)
}

// Until RunInstances seeds pointers, an instance whose variables are on an
// EFI volume still launches through the volume, and a BIOS or microvm
// guest needs no store at all.
func TestCheckUEFIVarStore_Accepts(t *testing.T) {
	tests := []struct {
		name string
		v    *VM
	}{
		{name: "EFI volume", v: &VM{BootMode: "uefi", EBSRequests: EBSRequests{Requests: []EBSRequest{{Name: "vol-efi", EFI: true}}}}},
		{name: "pointer", v: &VM{BootMode: "uefi-preferred", UEFIVarStore: &UEFIVarStoreRef{Key: "k"}}},
		{name: "BIOS", v: &VM{BootMode: "legacy-bios"}},
		{name: "no boot mode", v: &VM{}},
		{name: "microvm", v: &VM{BootMode: "uefi", DirectBoot: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, CheckUEFIVarStore(tt.v))
		})
	}
}

// A launch from a pointer opens this node's copy as pflash unit 1 with
// format=raw, after the read-only CODE blob.
func TestStartQEMU_PointerPassesTheLocalCopyAsPflashUnit1(t *testing.T) {
	f := newUEFIFixture(t)
	f.m.deps.NetworkPlumber = &fakeNetworkPlumber{}
	launches := withFakeQEMU(t)
	key := keyFor("boot")
	f.writeLocal(t, key, region("seeded"))
	instance := f.instance(key, f.identity)
	instance.EBSRequests.Requests = []EBSRequest{{Name: "vol-root", NBDURI: nbdListener(t), Boot: true}}

	require.NoError(t, f.m.startQEMU(instance, false))

	got := launches()
	require.Len(t, got, 1)
	vars := filepath.Join(f.instanceDir(), "vars.fd")
	assert.Equal(t, []string{
		"file=" + f.codePath + ",format=raw,if=pflash,unit=0,readonly=on",
		"file=" + vars + ",format=raw,if=pflash,unit=1",
	}, allArgValues(got[0], "-drive"))
}

// A reboot reuses the copy the launch seeded; without one there is nothing
// safe to boot, so QEMU is not started.
func TestStartQEMU_PointerWithoutALocalCopyIsNotLaunched(t *testing.T) {
	f := newUEFIFixture(t)
	f.m.deps.NetworkPlumber = &fakeNetworkPlumber{}
	launches := withFakeQEMU(t)
	instance := f.instance(keyFor("boot"), f.identity)
	instance.EBSRequests.Requests = []EBSRequest{{Name: "vol-root", NBDURI: nbdListener(t), Boot: true}}

	err := f.m.startQEMU(instance, false)

	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, launches())
}

func TestUEFIInstanceDir_RefusesIDsThatAreNotOnePathSegment(t *testing.T) {
	for _, id := range []string{"", "..", "i-../x", "i-a/b", `i-a\b`, "vol-1"} {
		_, err := uefiInstanceDir("/data", id)
		assert.Error(t, err, id)
	}
	dir, err := uefiInstanceDir("/data", testUEFIInstance)
	require.NoError(t, err)
	assert.Equal(t, "/data/uefi/"+testUEFIInstance, dir)
}

// The CODE hash is what a store records; a package update that replaces the
// blob must be hashed again rather than served from the cache.
func TestNodeFirmwareFor_HashesTheCodeBlobAgainAfterItChanges(t *testing.T) {
	f := newUEFIFixture(t)
	first, err := NodeFirmwareFor("x86_64")
	require.NoError(t, err)
	assert.Equal(t, f.identity, first.Identity)

	updated := []byte("ovmf code build 2, a longer blob")
	require.NoError(t, os.WriteFile(f.codePath, updated, 0o600))
	second, err := NodeFirmwareFor("x86_64")
	require.NoError(t, err)

	sum := sha256.Sum256(updated)
	assert.Equal(t, hex.EncodeToString(sum[:]), second.Identity.CodeSHA256)
	assert.Equal(t, f.codePath, second.Code)
}
