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
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvsync"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/nats-io/nats.go/jetstream"
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
// key has value, or of every point when key is empty.
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
				if v, ok := dp.Attributes.Value(attribute.Key(key)); key == "" || ok && v.AsString() == value {
					total += dp.Value
				}
			}
		}
	}
	return total
}

// uefiFixture is node-a with one x86_64 firmware candidate, an in-memory
// object store, a data directory, and the store records and claims of a real
// JetStream, with every other node's heartbeat set by the test.
type uefiFixture struct {
	m        *Manager
	mounter  *fakeVolumeMounter
	objects  *objectstore.MemoryObjectStore
	records  *uefistore.Store
	js       jetstream.JetStream
	liveness *fakeLiveness
	dataDir  string
	codePath string
	identity uefistore.FirmwareIdentity
}

// fakeLiveness answers LastSeen for each node from what the test set; a node
// it has no answer for has never heartbeated.
type fakeLiveness struct {
	mu   sync.Mutex
	seen map[string]time.Time
	err  error
}

func (l *fakeLiveness) LastSeen(_ context.Context, node string) (time.Time, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return time.Time{}, false, l.err
	}
	seen, ok := l.seen[node]
	return seen, ok, nil
}

func (l *fakeLiveness) heartbeat(node string, age time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[node] = time.Now().Add(-age)
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

	_, _, js := testutil.StartTestJetStream(t)
	records := uefistore.NewStore(js)
	require.NoError(t, records.Open(t.Context()))

	sum := sha256.Sum256(codeBytes)
	f := &uefiFixture{
		mounter:  &fakeVolumeMounter{},
		objects:  objectstore.NewMemoryObjectStore(),
		records:  records,
		js:       js,
		liveness: &fakeLiveness{seen: map[string]time.Time{}},
		dataDir:  t.TempDir(),
		codePath: code,
		identity: uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: testUEFIVarsSize, CodeSHA256: hex.EncodeToString(sum[:])},
	}
	f.m = NewManagerWithDeps(Deps{
		NodeID:        "node-a",
		VolumeMounter: f.mounter,
		InstanceTypes: fakeInstanceTypeResolver{"t3.nano": {Architecture: "x86_64", VCPUs: 1, MemoryMiB: 512}},
		UEFIVarStore: UEFIVarStoreDeps{
			Objects: f.objects, Bucket: testUEFIBucket, DataDir: f.dataDir,
			Records: records, Claims: uefistore.NewClaims(kvstore.OpenSyncBucket(js), "node-a"), Liveness: f.liveness,
		},
	})
	return f
}

// peerClaim is the instance's claim as another node sees it.
func (f *uefiFixture) peerClaim(node string) *kvsync.Claim {
	return uefistore.NewClaims(kvstore.OpenSyncBucket(f.js), node).Get(testUEFIInstance)
}

// claimHolder is who holds the instance's claim, read as a third node would.
func (f *uefiFixture) claimHolder(t *testing.T) (kvsync.Token, bool) {
	t.Helper()
	tok, ok, err := f.peerClaim("observer").Holder(t.Context())
	require.NoError(t, err)
	return tok, ok
}

func (f *uefiFixture) record(t *testing.T) (*uefistore.UEFIStoreRecord, uint64) {
	t.Helper()
	rec, rev, err := f.records.Get(t.Context(), testUEFIInstance)
	require.NoError(t, err)
	return rec, rev
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

func (f *uefiFixture) writeLocal(t *testing.T, seededKey string, generation uint64, vars []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(f.instanceDir(), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), "vars.fd"), vars, 0o600))
	seed, err := json.Marshal(uefiSeed{Key: seededKey, Generation: generation, SeededAt: time.Now()})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), "seeded.json"), seed, 0o600))
}

func (f *uefiFixture) localVars(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.instanceDir(), "vars.fd"))
	require.NoError(t, err)
	return b
}

func (f *uefiFixture) seed(t *testing.T) uefiSeed {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.instanceDir(), "seeded.json"))
	require.NoError(t, err)
	var seed uefiSeed
	require.NoError(t, json.Unmarshal(b, &seed))
	return seed
}

// instance is a stopped UEFI instance with no EFI volume. A non-empty key
// gives it a live store record pointing there, at generation 0 as a
// conversion leaves it.
func (f *uefiFixture) instance(t *testing.T, key string, fw uefistore.FirmwareIdentity) *VM {
	t.Helper()
	v := &VM{
		ID: testUEFIInstance, InstanceType: "t3.nano", BootMode: "uefi", Status: StateStopped,
		Instance: &ec2.Instance{},
	}
	if key != "" {
		_, err := f.records.Create(t.Context(), &uefistore.UEFIStoreRecord{
			InstanceID: testUEFIInstance, AccountID: "111122223333", State: uefistore.StateLive,
			Pointer: &uefistore.UEFIVarStoreRef{Key: key, Firmware: fw, WriterNode: "node-c", WrittenAt: time.Unix(1700000000, 0).UTC()},
		})
		require.NoError(t, err)
	}
	f.m.Insert(v)
	return v
}

// prepare runs the launch's store check and store step and returns the claim
// the step acquired.
func (f *uefiFixture) prepare(t *testing.T, v *VM) (*uefiClaim, error) {
	t.Helper()
	rec, err := f.m.checkUEFIVarStore(t.Context(), v)
	if err != nil {
		return nil, err
	}
	return f.m.prepareUEFIVarStore(t.Context(), v, rec)
}

func keyFor(name string) string { return uefistore.KeyPrefix(testUEFIInstance) + name }

func TestPrepareUEFIVarStore_ClaimsStampsAndFetchesTheObject(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("11111111-1111-1111-1111-111111111111")
	f.putObject(t, key, gzipped(t, region("converted")))
	instance := f.instance(t, key, f.identity)
	before := uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "object")

	held, err := f.prepare(t, instance)
	require.NoError(t, err)

	holder, ok := f.claimHolder(t)
	require.True(t, ok, "the claim is held from before the guest boots")
	assert.Equal(t, "node-a", holder.Holder)
	assert.Equal(t, held.token, holder)
	rec, _ := f.record(t)
	assert.Equal(t, holder.Generation, rec.Generation, "the record is stamped with the claim's generation")
	assert.Equal(t, key, rec.Pointer.Key, "a launch only reads the pointer")
	assert.Equal(t, region("converted"), f.localVars(t), "vars.fd must hold the decompressed region")
	assert.Equal(t, uefiSeed{
		Key: key, Generation: holder.Generation, SeededAt: f.seed(t).SeededAt, Firmware: f.identity, CodePath: f.codePath,
		Sum: uefiRegionSum(region("converted")),
	}, f.seed(t))
	assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "object"))
}

// A daemon restart or host reboot adopts the claim at its generation, and the
// file it left may hold changes no object has yet, so it is booted without a
// get and the record keeps the claim's generation.
func TestPrepareUEFIVarStore_AdoptedClaimReusesTheLocalCopy(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("22222222-2222-2222-2222-222222222222")
	instance := f.instance(t, key, f.identity)
	held, err := f.peerClaim("node-a").TryLock(t.Context())
	require.NoError(t, err)
	_, err = f.records.Stamp(t.Context(), testUEFIInstance, held.Generation)
	require.NoError(t, err)
	f.writeLocal(t, key, held.Generation, region("guest wrote this"))
	before := uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "local")

	got, err := f.prepare(t, instance)

	require.NoError(t, err, "no object exists, so a get would fail")
	assert.True(t, got.reused)
	assert.Equal(t, held, got.token, "a restart adopts the claim at the same generation")
	rec, _ := f.record(t)
	assert.Equal(t, held.Generation, rec.Generation, "adoption keeps the claim's generation")
	assert.Equal(t, region("guest wrote this"), f.localVars(t))
	assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "local"))
}

// Once the pointer or the generation has moved past what this node seeded,
// the local copy is stale and the pointed object replaces it. A copy from an
// earlier generation predates another node's claim or a restore.
func TestPrepareUEFIVarStore_RefetchesWhenTheLocalCopyIsNotTheCurrentStore(t *testing.T) {
	tests := []struct {
		name  string
		local func(f *uefiFixture, t *testing.T)
	}{
		{name: "seeded from an older pointer", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("older"), 0, region("stale"))
		}},
		{name: "seeded under an earlier generation", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("current"), 1, region("stale"))
		}},
		{name: "seeded before generations were recorded", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("current"), 0, region("stale"))
			require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), "seeded.json"), []byte(`{"key":"`+keyFor("current")+`"}`), 0o600))
		}},
		{name: "vars.fd of the wrong size", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("current"), 0, []byte("short"))
		}},
		{name: "seeded.json unreadable with no vars.fd", local: func(f *uefiFixture, t *testing.T) {
			require.NoError(t, os.MkdirAll(f.instanceDir(), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(f.instanceDir(), "seeded.json"), []byte("{"), 0o600))
		}},
		{name: "no vars.fd", local: func(f *uefiFixture, t *testing.T) {
			f.writeLocal(t, keyFor("current"), 0, region("stale"))
			require.NoError(t, os.Remove(filepath.Join(f.instanceDir(), "vars.fd")))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			key := keyFor("current")
			tt.local(f, t)
			f.putObject(t, key, gzipped(t, region("restored")))
			instance := f.instance(t, key, f.identity)

			held, err := f.prepare(t, instance)
			require.NoError(t, err)

			assert.False(t, held.reused)
			assert.Equal(t, region("restored"), f.localVars(t))
			assert.Equal(t, key, f.seed(t).Key)
			assert.Equal(t, held.token.Generation, f.seed(t).Generation)
		})
	}
}

// A store pflash cannot load fails the launch before any volume is mounted,
// names both firmwares, gives the claim back and leaves the object, the
// pointer and the local copy as they were.
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
			f.writeLocal(t, keyFor("older"), 0, region("local"))
			stored := f.identity
			tt.mutate(&stored)
			instance := f.instance(t, key, stored)
			wantRef, _ := f.record(t)
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
			rec, _ := f.record(t)
			assert.Equal(t, wantRef.Pointer, rec.Pointer, "the pointer is unchanged")
			assert.Equal(t, body, f.objectBody(t, key), "the object is unchanged")
			assert.Equal(t, region("local"), f.localVars(t), "the local copy is unchanged")
			assert.Equal(t, keyFor("older"), f.seed(t).Key)
			_, held := f.claimHolder(t)
			assert.False(t, held, "a launch that never started QEMU gives the claim back")
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
	instance := f.instance(t, key, stored)
	before := uefiCounter(t, "mulga.vm.uefi_store.firmware_mismatches", "kind", "code")

	_, err := f.prepare(t, instance)
	require.NoError(t, err)

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
	instance := f.instance(t, keyFor("down"), f.identity)

	_, err := f.prepare(t, instance)

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
			instance := f.instance(t, key, f.identity)

			_, err := f.prepare(t, instance)

			require.ErrorIs(t, err, ErrUEFIVarStoreUnusable)
			require.NotErrorIs(t, err, ErrMountRetryable)
			assert.NoFileExists(t, filepath.Join(f.instanceDir(), "vars.fd"))
			assert.NoFileExists(t, filepath.Join(f.instanceDir(), "seeded.json"))
		})
	}
}

// A record this node cannot trust, such as one pointing outside the
// instance's prefix, is refused for good before any claim is taken.
func TestRun_UntrustedStoreRecordIsRefused(t *testing.T) {
	f := newUEFIFixture(t)
	instance := f.instance(t, "", f.identity)
	kv, err := f.js.KeyValue(t.Context(), uefistore.BucketName)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), testUEFIInstance, []byte(`{"schema_version":1,"instance_id":"`+testUEFIInstance+
		`","state":"live","pointer":{"key":"uefi/i-0aaaaaaaaaaaaaaaa/other","firmware":{"arch":"x86_64","vars_size":4096},"writer_node":"n"}}`))
	require.NoError(t, err)

	err = f.m.Run(t.Context(), instance)

	require.ErrorIs(t, err, ErrUEFIVarStoreUnusable)
	require.NotErrorIs(t, err, ErrMountRetryable)
	_, held := f.claimHolder(t)
	assert.False(t, held)
}

// A BIOS instance and a microvm touch neither the claim, the object store nor
// the data directory.
func TestPrepareUEFIVarStore_NoStoreObjectIsANoOp(t *testing.T) {
	f := newUEFIFixture(t)
	f.m.deps.UEFIVarStore.Objects = &failingGetStore{MemoryObjectStore: f.objects, err: errors.New("must not be called")}
	bios := f.instance(t, keyFor("bios"), f.identity)
	bios.BootMode = ""
	held, err := f.prepare(t, bios)
	require.NoError(t, err)
	assert.Nil(t, held)

	microvm := &VM{ID: testUEFIInstance, BootMode: "uefi", DirectBoot: true}
	held, err = f.prepare(t, microvm)
	require.NoError(t, err)
	assert.Nil(t, held)

	_, claimed := f.claimHolder(t)
	assert.False(t, claimed)
	assert.NoDirExists(t, filepath.Join(f.dataDir, "uefi"))
}

// A UEFI instance with no usable store record, or one still carrying an EFI
// volume, is refused before the launch clears or changes anything on its
// record, and before any claim is taken.
func TestRun_UEFIInstanceWithoutExactlyOneStoreIsRefused(t *testing.T) {
	efiVolume := EBSRequest{Name: "vol-1-efi", EFI: true}
	tests := []struct {
		name   string
		record func(f *uefiFixture, t *testing.T)
		efi    bool
		want   error
	}{
		{name: "no record and no EFI volume", want: ErrUEFIVarStoreMissing},
		{name: "a record with no pointer", want: ErrUEFIVarStoreMissing, record: func(f *uefiFixture, t *testing.T) {
			_, err := f.records.Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: testUEFIInstance, AccountID: "a", State: uefistore.StateLive})
			require.NoError(t, err)
		}},
		{name: "a deleting record", want: ErrUEFIVarStoreMissing, record: func(f *uefiFixture, t *testing.T) {
			f.instance(t, keyFor("p"), f.identity)
			_, err := f.records.MarkDeleting(t.Context(), testUEFIInstance)
			require.NoError(t, err)
		}},
		{name: "an EFI volume and no record", efi: true, want: ErrUEFIEFIVolume},
		{name: "a record and an EFI volume", efi: true, want: ErrUEFIEFIVolume, record: func(f *uefiFixture, t *testing.T) {
			f.instance(t, keyFor("p"), f.identity)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			if tt.record != nil {
				tt.record(f, t)
			}
			instance := f.instance(t, "", f.identity)
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
			_, held := f.claimHolder(t)
			assert.False(t, held, "a refused start takes no claim")
		})
	}
}

// Start from StateError transitions to pending before launching; a refused
// UEFI instance must not take that transition either.
func TestStart_UEFIInstanceWithoutAStoreIsRefusedBeforeTheTransition(t *testing.T) {
	f := newUEFIFixture(t)
	transitions := 0
	f.m.deps.TransitionState = func(v *VM, s InstanceState) error { transitions++; v.Status = s; return nil }
	instance := f.instance(t, "", f.identity)
	instance.Status = StateError

	err := f.m.Start(t.Context(), instance.ID)

	require.ErrorIs(t, err, ErrUEFIVarStoreMissing)
	assert.Equal(t, 0, transitions)
	assert.Equal(t, StateError, instance.Status)
}

// A UEFI instance with a live store record that points at an object launches,
// and a BIOS or microvm guest needs no store at all, even where no store
// records are wired.
func TestCheckUEFIVarStore_Accepts(t *testing.T) {
	tests := []struct {
		name       string
		v          func(f *uefiFixture, t *testing.T) *VM
		unwir      bool
		unreadable bool
	}{
		{name: "BIOS on a node with no store records", unwir: true, v: func(*uefiFixture, *testing.T) *VM {
			return &VM{ID: testUEFIInstance, BootMode: "legacy-bios"}
		}},
		{name: "BIOS while the store bucket cannot be read", unreadable: true, v: func(*uefiFixture, *testing.T) *VM {
			return &VM{ID: testUEFIInstance, BootMode: "legacy-bios"}
		}},
		{name: "store record", v: func(f *uefiFixture, t *testing.T) *VM {
			v := f.instance(t, keyFor("k"), f.identity)
			v.BootMode = "uefi-preferred"
			return v
		}},
		{name: "BIOS", v: func(*uefiFixture, *testing.T) *VM { return &VM{ID: testUEFIInstance, BootMode: "legacy-bios"} }},
		{name: "no boot mode", v: func(*uefiFixture, *testing.T) *VM { return &VM{ID: testUEFIInstance} }},
		{name: "microvm", v: func(*uefiFixture, *testing.T) *VM {
			return &VM{ID: testUEFIInstance, BootMode: "uefi", DirectBoot: true}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUEFIFixture(t)
			if tt.unwir {
				f.m.deps.UEFIVarStore.Records = nil
			}
			if tt.unreadable {
				f.m.deps.UEFIVarStore.Records = failingRecordGet{f.records}
			}
			assert.NoError(t, f.m.CheckUEFIVarStore(t.Context(), tt.v(f, t)))
		})
	}
}

// A store-record launch opens this node's copy as pflash unit 1 with
// format=raw, after the read-only CODE blob.
func TestStartQEMU_StoreObjectPassesTheLocalCopyAsPflashUnit1(t *testing.T) {
	f := newUEFIFixture(t)
	f.m.deps.NetworkPlumber = &fakeNetworkPlumber{}
	launches := withFakeQEMU(t)
	key := keyFor("boot")
	f.writeLocal(t, key, 1, region("seeded"))
	instance := f.instance(t, key, f.identity)
	instance.EBSRequests.Requests = []EBSRequest{{Name: "vol-root", NBDURI: nbdListener(t), Boot: true}}

	require.NoError(t, f.m.startQEMU(instance))

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
func TestStartQEMU_StoreObjectWithoutALocalCopyIsNotLaunched(t *testing.T) {
	f := newUEFIFixture(t)
	f.m.deps.NetworkPlumber = &fakeNetworkPlumber{}
	launches := withFakeQEMU(t)
	instance := f.instance(t, keyFor("boot"), f.identity)
	instance.EBSRequests.Requests = []EBSRequest{{Name: "vol-root", NBDURI: nbdListener(t), Boot: true}}

	err := f.m.startQEMU(instance)

	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Empty(t, launches())
}

// A launch that fails before QEMU starts gives back a claim whose store it
// fetched, since nothing on this node is newer than the object.
func TestRun_LaunchFailingBeforeQEMUReleasesAFetchedClaim(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("fetched")
	f.putObject(t, key, gzipped(t, region("object")))
	instance := f.instance(t, key, f.identity)
	f.mounter.mountErr = errors.New("mount refused")

	require.Error(t, f.m.Run(t.Context(), instance))

	_, held := f.claimHolder(t)
	assert.False(t, held, "another node may start the instance at once")
}

// A launch that booted this node's copy keeps the claim when it fails, because
// that copy may hold variable changes no object has.
func TestRun_LaunchFailingAfterReusingTheLocalCopyKeepsTheClaim(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("reused")
	instance := f.instance(t, key, f.identity)
	tok, err := f.peerClaim("node-a").TryLock(t.Context())
	require.NoError(t, err)
	_, err = f.records.Stamp(t.Context(), testUEFIInstance, tok.Generation)
	require.NoError(t, err)
	f.writeLocal(t, key, tok.Generation, region("unlanded"))
	f.mounter.mountErr = errors.New("mount refused")

	require.Error(t, f.m.Run(t.Context(), instance))

	holder, held := f.claimHolder(t)
	require.True(t, held)
	assert.Equal(t, tok, holder)
}

// A recreated sync bucket restarts generations, so a store record can sit
// above every new token. The start is refused as retryable and the record
// keeps its generation and pointer.
func TestRun_RecordAboveTheClaimGenerationIsRefusedAndUnchanged(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("fenced")
	f.putObject(t, key, gzipped(t, region("object")))
	instance := f.instance(t, key, f.identity)
	rec, rev := f.record(t)
	rec.Generation = 1 << 40
	_, err := f.records.CompareAndSet(t.Context(), rec, rev)
	require.NoError(t, err)
	want, wantRev := f.record(t)
	before := uefiCounter(t, "mulga.vm.uefi_store.claim_refusals", "reason", "fenced")

	err = f.m.Run(t.Context(), instance)

	require.ErrorIs(t, err, ErrUEFIClaimHeld)
	require.ErrorIs(t, err, ErrMountRetryable)
	got, gotRev := f.record(t)
	assert.Equal(t, wantRev, gotRev, "the record is not written")
	assert.Equal(t, want, got)
	assert.Empty(t, f.mounter.mounted)
	assert.NoFileExists(t, filepath.Join(f.instanceDir(), "vars.fd"))
	assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.claim_refusals", "reason", "fenced"))
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

// failingRecordGet is a store record bucket whose reads fail.
type failingRecordGet struct{ UEFIStoreRecords }

func (failingRecordGet) Get(context.Context, string) (*uefistore.UEFIStoreRecord, uint64, error) {
	return nil, 0, errors.New("uefi stores: no responders")
}

// At one generation only this node moves the pointer, and only from vars.fd,
// so a crash between a move and the seeded.json rewrite leaves a file newer
// than the key seeded.json names. It is booted, not refetched over.
func TestPrepareUEFIVarStore_ReusesTheCopyOfTheClaimsGenerationWhateverItsKey(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("moved")
	f.putObject(t, key, gzipped(t, region("object")))
	instance := f.instance(t, key, f.identity)
	tok, err := f.peerClaim("node-a").TryLock(t.Context())
	require.NoError(t, err)
	f.writeLocal(t, keyFor("before-move"), tok.Generation, region("newer than the object"))

	held, err := f.prepare(t, instance)

	require.NoError(t, err)
	assert.True(t, held.reused)
	assert.Equal(t, region("newer than the object"), f.localVars(t))
}

// A launch that fails inside startQEMU before the process starts ran no
// guest, so it gives back a claim it fetched the store under.
func TestRun_LaunchFailingBeforeQEMUExecsReleasesAFetchedClaim(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("fetched")
	f.putObject(t, key, gzipped(t, region("object")))
	instance := f.instance(t, key, f.identity)

	err := f.m.Run(t.Context(), instance)

	require.ErrorIs(t, err, errQEMUNotStarted, "an instance with no boot volume fails before QEMU execs")
	_, held := f.claimHolder(t)
	assert.False(t, held, "another node may start the instance at once")
}

// A node that cannot boot the store refuses before it touches the claim, so
// it never takes the store from a holder that is coming back.
func TestRun_FirmwareMismatchRefusesBeforeTakingTheClaim(t *testing.T) {
	f := newUEFIFixture(t)
	key := keyFor("arm")
	stored := f.identity
	stored.Arch = "arm64"
	instance := f.instance(t, key, stored)
	tok, err := f.peerClaim("node-b").TryLock(t.Context())
	require.NoError(t, err)
	f.liveness.heartbeat("node-b", 10*time.Minute)

	err = f.m.Run(t.Context(), instance)

	require.ErrorIs(t, err, ErrUEFIFirmwareMismatch)
	holder, held := f.claimHolder(t)
	require.True(t, held)
	assert.Equal(t, tok, holder, "the claim stays with node-b")
	rec, _ := f.record(t)
	assert.Zero(t, rec.Generation, "the record is not stamped")
}
