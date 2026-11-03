package instance

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	awserrors "github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/state/kvstore"
	ebsprovider "github.com/mulgadc/spinifex/spinifex/providers/ebs"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testSeedInstance = "i-0a1b2c3d4e5f60718"
	testSeedNode     = "node-a"
	testSeedType     = "t3.nano"
)

// recordingObjects keeps each put's metadata, which the memory store drops,
// and can fail store puts or check the store record exists when one lands.
type recordingObjects struct {
	*objectstore.MemoryObjectStore

	mu       sync.Mutex
	metadata map[string]map[string]string
	putErr   error
	onPut    func(key string)
}

var _ objectstore.ObjectStore = (*recordingObjects)(nil)

func (r *recordingObjects) PutObject(ctx context.Context, in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	if r.onPut != nil {
		r.onPut(aws.StringValue(in.Key))
	}
	if r.putErr != nil && strings.HasPrefix(aws.StringValue(in.Key), "uefi/") {
		return nil, r.putErr
	}
	r.mu.Lock()
	r.metadata[aws.StringValue(in.Key)] = aws.StringValueMap(in.Metadata)
	r.mu.Unlock()
	return r.MemoryObjectStore.PutObject(ctx, in)
}

type seedFixture struct {
	svc      *InstanceServiceImpl
	provider *ebsprovider.MemoryProvider
	objects  *recordingObjects
	records  *uefistore.Store
	template []byte
	codePath string
	identity uefistore.FirmwareIdentity
}

func newSeedFixture(t *testing.T) *seedFixture {
	t.Helper()
	fwDir := t.TempDir()
	code := filepath.Join(fwDir, "OVMF_CODE.fd")
	vars := filepath.Join(fwDir, "OVMF_VARS.fd")
	codeBytes := []byte("ovmf code build 7")
	template := bytes.Repeat([]byte("VARS"), 1024)
	require.NoError(t, os.WriteFile(code, codeBytes, 0o600))
	require.NoError(t, os.WriteFile(vars, template, 0o600))
	orig := vm.FirmwarePathCandidates
	vm.FirmwarePathCandidates = map[string][]vm.FirmwareCandidate{"x86_64": {{Code: code, VarsTemplate: vars}}} //nolint:reassign // exported for tests to swap, restored below

	t.Cleanup(func() { vm.FirmwarePathCandidates = orig }) //nolint:reassign // restoring the candidates captured above

	_, _, js := testutil.StartTestJetStream(t)
	records := uefistore.NewStore(js)
	require.NoError(t, records.Open(t.Context()))

	provider := ebsprovider.NewMemoryProvider(ebsprovider.Capabilities{CrashConsistentSnapshot: true, VolumeSeeding: true, VolumeEnumeration: true})
	seedProviderSnapshot(t, provider, "vol-origin", "snap-source")
	objects := &recordingObjects{MemoryObjectStore: objectstore.NewMemoryObjectStore(), metadata: map[string]map[string]string{}}
	svc := providerRootVolumeService(t, provider, rootVolumeAMILoader(), objects)
	svc.vmMgr = vm.NewManager()
	svc.instanceTypes = map[string]*ec2.InstanceTypeInfo{testSeedType: {
		InstanceType:  aws.String(testSeedType),
		ProcessorInfo: &ec2.ProcessorInfo{SupportedArchitectures: []*string{aws.String("x86_64")}},
	}}
	svc.SetUEFIVarStore(records, testSeedNode)

	sum := sha256.Sum256(codeBytes)
	return &seedFixture{
		svc: svc, provider: provider, objects: objects, records: records, template: template, codePath: code,
		identity: uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: int64(len(template)), CodeSHA256: hex.EncodeToString(sum[:])},
	}
}

func (f *seedFixture) instance(bootMode string) *vm.VM {
	v := &vm.VM{ID: testSeedInstance, AccountID: testRootAccount, InstanceType: testSeedType, BootMode: bootMode, Instance: &ec2.Instance{}}
	f.svc.vmMgr.Insert(v)
	return v
}

func seedRunInput() *ec2.RunInstancesInput {
	return &ec2.RunInstancesInput{ImageId: aws.String("ami-1"), InstanceType: aws.String(testSeedType)}
}

func (f *seedFixture) objectKeys(t *testing.T) []string {
	t.Helper()
	out, err := f.objects.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket: aws.String(testRootBucket), Prefix: aws.String(uefistore.KeyPrefix(testSeedInstance)),
	})
	require.NoError(t, err)
	keys := make([]string, 0, len(out.Contents))
	for _, o := range out.Contents {
		keys = append(keys, aws.StringValue(o.Key))
	}
	return keys
}

func (f *seedFixture) providerVolumeIDs(t *testing.T) []string {
	t.Helper()
	out, err := f.provider.ListVolumes(t.Context(), ebsprovider.ListVolumesRequest{Versioned: ebsprovider.NewVersioned()})
	require.NoError(t, err)
	ids := make([]string, 0, len(out.Volumes))
	for _, v := range out.Volumes {
		ids = append(ids, v.ID)
	}
	return ids
}

// A UEFI launch seeds its store as an object and a pointer: no <root>-efi
// volume, no EFI request, a live store record, and a gzip of this node's
// template whose metadata and pointer name the firmware and the writer.
func TestGenerateVolumes_UEFISeedsTheStoreFromTheTemplate(t *testing.T) {
	f := newSeedFixture(t)
	instance := f.instance("uefi")

	infos, err := f.svc.GenerateVolumes(t.Context(), seedRunInput(), instance)
	require.NoError(t, err)

	require.Len(t, infos, 1, "only the root volume is reported")
	for _, id := range f.providerVolumeIDs(t) {
		assert.False(t, strings.HasSuffix(id, "-efi"), "no EFI volume is created, found %s", id)
	}
	for _, r := range instance.EBSRequests.Requests {
		assert.False(t, r.EFI, "no EFI request is appended, found %s", r.Name)
	}

	ref := instance.UEFIVarStore
	require.NotNil(t, ref, "the launch's record carries a pointer")
	assert.Equal(t, []string{ref.Key}, f.objectKeys(t), "the pointer names the one object under the prefix")
	assert.Equal(t, f.identity, ref.Firmware)
	assert.Equal(t, testSeedNode, ref.WriterNode)
	assert.False(t, ref.WrittenAt.IsZero())
	assert.Equal(t, ref, instance.Record().Status.UEFIVarStore, "the pointer is in the record's status")

	out, err := f.objects.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(testRootBucket), Key: aws.String(ref.Key)})
	require.NoError(t, err)
	defer out.Body.Close()
	gz, err := gzip.NewReader(out.Body)
	require.NoError(t, err, "the body is gzip")
	region, err := io.ReadAll(gz)
	require.NoError(t, err)
	assert.Equal(t, f.template, region, "the store starts as this node's VARS template")

	assert.Equal(t, map[string]string{
		"instance-id":          testSeedInstance,
		"writer-node":          testSeedNode,
		"firmware-arch":        "x86_64",
		"firmware-vars-size":   strconv.Itoa(len(f.template)),
		"firmware-code-sha256": f.identity.CodeSHA256,
		"firmware-code-path":   f.codePath,
		"source":               "template",
	}, f.objects.metadata[ref.Key])

	rec, _, err := f.records.Get(t.Context(), testSeedInstance)
	require.NoError(t, err)
	assert.Equal(t, uefistore.StateLive, rec.State)
	assert.Equal(t, testRootAccount, rec.AccountID)
	assert.Nil(t, rec.WriteBack)
}

// The store record exists before the object is put, so terminate can reach
// every object under the prefix, including one whose put failed midway.
func TestGenerateVolumes_UEFIRecordsTheStoreBeforeItsObject(t *testing.T) {
	f := newSeedFixture(t)
	instance := f.instance("uefi")
	recordAtPut := errors.New("no put")
	f.objects.onPut = func(key string) {
		if strings.HasPrefix(key, uefistore.KeyPrefix(testSeedInstance)) {
			_, _, recordAtPut = f.records.Get(context.Background(), testSeedInstance)
		}
	}

	_, err := f.svc.GenerateVolumes(t.Context(), seedRunInput(), instance)

	require.NoError(t, err)
	assert.NoError(t, recordAtPut, "the record is live when the object is put")
}

// A failed put fails the launch with InternalError and sets no pointer, and
// leaves the record so terminate can still settle the prefix.
func TestGenerateVolumes_UEFIPutFailureFailsTheLaunch(t *testing.T) {
	f := newSeedFixture(t)
	instance := f.instance("uefi")
	f.objects.putErr = errors.New("predastore unreachable")

	_, err := f.svc.GenerateVolumes(t.Context(), seedRunInput(), instance)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Nil(t, instance.UEFIVarStore)
	rec, _, getErr := f.records.Get(t.Context(), testSeedInstance)
	require.NoError(t, getErr)
	assert.Equal(t, uefistore.StateLive, rec.State)
}

// A record already under the instance's ID is not this launch's, so the
// launch writes nothing under its prefix.
func TestGenerateVolumes_UEFIExistingRecordFailsTheLaunch(t *testing.T) {
	f := newSeedFixture(t)
	instance := f.instance("uefi")
	_, err := f.records.Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: testSeedInstance, AccountID: "000000000099", State: uefistore.StateLive})
	require.NoError(t, err)

	_, err = f.svc.GenerateVolumes(t.Context(), seedRunInput(), instance)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Empty(t, f.objectKeys(t))
	assert.Nil(t, instance.UEFIVarStore)
}

// A BIOS guest has no variable store: no record, no object, no pointer.
func TestGenerateVolumes_BIOSSeedsNoStore(t *testing.T) {
	f := newSeedFixture(t)
	instance := f.instance("legacy-bios")

	_, err := f.svc.GenerateVolumes(t.Context(), seedRunInput(), instance)

	require.NoError(t, err)
	assert.Nil(t, instance.UEFIVarStore)
	assert.Empty(t, f.objectKeys(t))
	_, _, err = f.records.Get(t.Context(), testSeedInstance)
	assert.ErrorIs(t, err, kvstore.ErrNotFound)
}

// A node without the store wired cannot seed, so it fails the launch rather
// than booting a UEFI guest with no store.
func TestGenerateVolumes_UEFIWithoutStoreWiringFails(t *testing.T) {
	f := newSeedFixture(t)
	f.svc.SetUEFIVarStore(nil, "")
	instance := f.instance("uefi")

	_, err := f.svc.GenerateVolumes(t.Context(), seedRunInput(), instance)

	require.Error(t, err)
	assert.Equal(t, awserrors.ErrorServerInternal, err.Error())
	assert.Nil(t, instance.UEFIVarStore)
}
