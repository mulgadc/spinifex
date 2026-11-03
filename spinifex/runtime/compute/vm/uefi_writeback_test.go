package vm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/instance/uefistore"
	"github.com/mulgadc/spinifex/spinifex/providers/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	nodeA = "node-a"
	nodeB = "node-b"
)

var errPredastoreDown = errors.New("dial tcp: connection refused")

// flakyObjects is the object store with puts that can be failed, the way an
// unreachable predastore fails them, and a hook run after each put lands.
type flakyObjects struct {
	*objectstore.MemoryObjectStore

	mu       sync.Mutex
	putErr   error
	afterPut func(key string)
	puts     int
	meta     map[string]map[string]string
}

func (o *flakyObjects) PutObject(ctx context.Context, in *s3.PutObjectInput) (*s3.PutObjectOutput, error) {
	o.mu.Lock()
	err, hook := o.putErr, o.afterPut
	o.puts++
	o.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out, err := o.MemoryObjectStore.PutObject(ctx, in)
	if err != nil {
		return out, err
	}
	o.mu.Lock()
	if o.meta == nil {
		o.meta = map[string]map[string]string{}
	}
	o.meta[aws.StringValue(in.Key)] = aws.StringValueMap(in.Metadata)
	o.mu.Unlock()
	if hook != nil {
		hook(aws.StringValue(in.Key))
	}
	return out, err
}

func (o *flakyObjects) putMetadata(key string) map[string]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.meta[key]
}

func (o *flakyObjects) putCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.puts
}

func (o *flakyObjects) setPutErr(err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.putErr = err
}

// wbStateStore is fakeStateStore holding the stopped record as KV does, a
// copy of the VM, and whose write can fail outright or land without a reply.
type wbStateStore struct {
	*fakeStateStore

	stoppedErr   error
	landThenFail bool
}

func (s *wbStateStore) WriteStoppedInstance(id string, v *VM) error {
	if s.stoppedErr != nil {
		return s.stoppedErr
	}
	if err := s.fakeStateStore.WriteStoppedInstance(id, VMFromRecord(v.Record())); err != nil {
		return err
	}
	if s.landThenFail {
		return errors.New("nats: timeout")
	}
	return nil
}

// fakeUEFIPointers answers for the cluster's instance records: a stopped one
// from the state store, a running one from the node the test says runs it.
type fakeUEFIPointers struct {
	mu      sync.Mutex
	states  *wbStateStore
	running map[string]*VM
	nodes   map[string]string
	revs    map[string]uint64
	loadErr error
}

var _ UEFIInstancePointers = (*fakeUEFIPointers)(nil)

func (p *fakeUEFIPointers) run(node string, v *VM) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.running[v.ID], p.nodes[v.ID] = v, node
}

func (p *fakeUEFIPointers) LoadUEFIVarStoreOwner(id string) (UEFIVarStoreOwner, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loadErr != nil {
		return UEFIVarStoreOwner{}, false, p.loadErr
	}
	if v, _ := p.states.LoadStoppedInstance(id); v != nil {
		return UEFIVarStoreOwner{Status: StateStopped, LastNode: v.LastNode, Pointer: v.UEFIVarStore, Revision: p.revs[id]}, true, nil
	}
	if v, ok := p.running[id]; ok {
		return UEFIVarStoreOwner{Status: StateRunning, LastNode: p.nodes[id], Pointer: v.UEFIVarStore, Revision: p.revs[id]}, true, nil
	}
	return UEFIVarStoreOwner{}, false, nil
}

func (p *fakeUEFIPointers) CompareAndSetUEFIVarStore(id string, ref *UEFIVarStoreRef, rev uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, _ := p.states.LoadStoppedInstance(id)
	if v == nil || p.revs[id] != rev {
		return fmt.Errorf("conflict on %s", id)
	}
	v.UEFIVarStore = ref
	p.revs[id]++
	return nil
}

// wbCluster is the shared side of a write-back: object store, store records,
// instance records. Each node has its own manager and data directory.
type wbCluster struct {
	objects  *flakyObjects
	records  *uefistore.Store
	states   *wbStateStore
	pointers *fakeUEFIPointers
	identity uefistore.FirmwareIdentity
	seedKey  string
}

type wbNode struct {
	c       *wbCluster
	name    string
	m       *Manager
	dataDir string
}

func newWBCluster(t *testing.T) *wbCluster {
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

	_, _, js := testutil.StartTestJetStream(t)
	records := uefistore.NewStore(js)
	require.NoError(t, records.Open(t.Context()))
	states := &wbStateStore{fakeStateStore: newFakeStateStore()}
	c := &wbCluster{
		objects:  &flakyObjects{MemoryObjectStore: objectstore.NewMemoryObjectStore()},
		records:  records,
		states:   states,
		pointers: &fakeUEFIPointers{states: states, running: map[string]*VM{}, nodes: map[string]string{}, revs: map[string]uint64{}},
		identity: uefistore.FirmwareIdentity{Arch: "x86_64", VarsSize: testUEFIVarsSize, CodeSHA256: hex.EncodeToString(sum[:])},
		seedKey:  keyFor("seed"),
	}
	_, err := records.Create(t.Context(), &uefistore.UEFIStoreRecord{InstanceID: testUEFIInstance, AccountID: "111122223333", State: uefistore.StateLive})
	require.NoError(t, err)
	c.put(t, c.seedKey, region("template"))
	return c
}

func (c *wbCluster) node(t *testing.T, name string) *wbNode {
	t.Helper()
	n := &wbNode{c: c, name: name, dataDir: t.TempDir()}
	n.m = NewManagerWithDeps(Deps{
		NodeID:        name,
		StateStore:    c.states,
		VolumeMounter: &fakeVolumeMounter{},
		InstanceTypes: fakeInstanceTypeResolver{"t3.nano": {Architecture: "x86_64", VCPUs: 1, MemoryMiB: 512}},
		UEFIVarStore: UEFIVarStoreDeps{
			Objects: c.objects, Bucket: testUEFIBucket, DataDir: n.dataDir,
			Records: c.records, Instances: c.pointers,
		},
	})
	return n
}

func (c *wbCluster) put(t *testing.T, key string, vars []byte) {
	t.Helper()
	_, err := c.objects.MemoryObjectStore.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(testUEFIBucket), Key: aws.String(key), Body: bytes.NewReader(gzipped(t, vars)),
	})
	require.NoError(t, err)
}

// keys lists every object under the instance's prefix.
func (c *wbCluster) keys(t *testing.T) []string {
	t.Helper()
	listed, _, err := objectstore.ListAll(t.Context(), c.objects, &s3.ListObjectsV2Input{
		Bucket: aws.String(testUEFIBucket), Prefix: aws.String(uefistore.KeyPrefix(testUEFIInstance)),
	})
	require.NoError(t, err)
	var keys []string
	for _, o := range listed {
		keys = append(keys, aws.StringValue(o.Key))
	}
	return keys
}

func (c *wbCluster) exists(t *testing.T, key string) bool {
	t.Helper()
	return slices.Contains(c.keys(t), key)
}

// region returns the decompressed region stored at key.
func (c *wbCluster) region(t *testing.T, key string) []byte {
	t.Helper()
	out, err := c.objects.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(testUEFIBucket), Key: aws.String(key)})
	require.NoError(t, err)
	defer out.Body.Close()
	zr, err := gzip.NewReader(out.Body)
	require.NoError(t, err)
	b, err := io.ReadAll(zr)
	require.NoError(t, err)
	return b
}

func (c *wbCluster) owed(t *testing.T) *uefistore.UEFIWriteBack {
	t.Helper()
	wb, err := c.records.LiveWriteBack(t.Context(), testUEFIInstance)
	require.NoError(t, err)
	return wb
}

func (c *wbCluster) stopped(t *testing.T) *VM {
	t.Helper()
	v, err := c.states.LoadStoppedInstance(testUEFIInstance)
	require.NoError(t, err)
	require.NotNil(t, v, "the stop must have written the stopped record")
	return v
}

// launch starts the instance on n from pointer ref, as a start does: the
// store is seeded or reused, then the guest writes vars.
func (n *wbNode) launch(t *testing.T, v *VM, vars []byte) *VM {
	t.Helper()
	v.Status, v.DesiredState = StateRunning, DesiredRunning
	n.m.Insert(v)
	n.c.pointers.run(n.name, v)
	require.NoError(t, n.m.prepareUEFIVarStore(t.Context(), v))
	n.guestWrites(t, vars)
	return v
}

func (n *wbNode) newInstance(t *testing.T, vars []byte) *VM {
	t.Helper()
	v := &VM{
		ID: testUEFIInstance, InstanceType: "t3.nano", BootMode: "uefi", Instance: &ec2.Instance{},
		UEFIVarStore: &UEFIVarStoreRef{Key: n.c.seedKey, Firmware: n.c.identity, WriterNode: n.name, WrittenAt: time.Now().UTC()},
	}
	return n.launch(t, v, vars)
}

// restart takes the stopped record and starts it on n again.
func (n *wbNode) restart(t *testing.T, vars []byte) *VM {
	t.Helper()
	v := n.c.stopped(t)
	require.NoError(t, n.c.states.DeleteStoppedInstance(v.ID))
	return n.launch(t, v, vars)
}

func (n *wbNode) guestWrites(t *testing.T, vars []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(n.dir(), uefiVarsFileName), vars, 0o600))
}

func (n *wbNode) dir() string { return filepath.Join(n.dataDir, "uefi", testUEFIInstance) }

func (n *wbNode) seeded(t *testing.T) string {
	t.Helper()
	key, err := readUEFISeed(n.dir())
	require.NoError(t, err)
	return key
}

func (n *wbNode) stop(t *testing.T, v *VM) {
	t.Helper()
	n.m.UpdateState(v.ID, func(v *VM) { v.DesiredState = DesiredStopped })
	require.NoError(t, n.m.Stop(v.ID))
}

func (n *wbNode) sweep(t *testing.T) int {
	t.Helper()
	settled, err := n.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.NoError(t, err)
	return settled
}

// Variables the guest wrote are in a new object the stopped record points
// at, and a start on the same node boots this node's copy without a get.
func TestUEFIWriteBack_StopWritesTheStoreBackAndAStartHereReusesIt(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("BootOrder=0001"))
	before := uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "written")

	a.stop(t, v)

	rec := c.stopped(t)
	require.NotNil(t, rec.UEFIVarStore)
	key := rec.UEFIVarStore.Key
	assert.NotEqual(t, c.seedKey, key, "a write-back puts to a fresh key, never over the pointed one")
	assert.Equal(t, region("BootOrder=0001"), c.region(t, key))
	assert.Equal(t, nodeA, rec.UEFIVarStore.WriterNode)
	assert.Equal(t, c.identity, rec.UEFIVarStore.Firmware)
	assert.Equal(t, key, a.seeded(t), "seeded.json names the written key so a start here reuses the copy")
	assert.Nil(t, c.owed(t))
	assert.True(t, c.exists(t, c.seedKey), "the version it replaced is retained")
	assert.Equal(t, before+1, uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "written"))

	reused := uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "local")
	a.restart(t, region("BootOrder=0001"))
	assert.Equal(t, reused+1, uefiCounter(t, "mulga.vm.uefi_store.seeds", "source", "local"))
}

// The written object carries the write-back source and its writer, so an
// overwrite by a superseded node is detectable from the object alone.
func TestUEFIWriteBack_ObjectRecordsItsSourceAndWriter(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	a.stop(t, a.newInstance(t, region("v1")))

	want, err := uefistore.ObjectMeta{OwnerID: testUEFIInstance, WriterNode: nodeA, Firmware: c.identity,
		CodePath: FirmwarePathCandidates["x86_64"][0].Code, Source: uefistore.SourceWriteBack}.Metadata()
	require.NoError(t, err)
	assert.Equal(t, aws.StringValueMap(want), c.objects.putMetadata(c.stopped(t).UEFIVarStore.Key))
}

// A drain stop keeps the instance in the running set, so its pointer is set
// in memory and every later running-set write carries it.
func TestUEFIWriteBack_DrainStopPointerSurvivesTheNextRunningSetWrite(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("drained"))

	require.NoError(t, a.m.StopAll())

	var key string
	a.m.Inspect(v, func(v *VM) { key = v.UEFIVarStore.Key })
	assert.NotEqual(t, c.seedKey, key)
	assert.Equal(t, region("drained"), c.region(t, key))
	assert.Equal(t, key, a.seeded(t))

	require.NoError(t, a.m.writeRunningState())
	saved, _, err := c.states.LoadRunningState(nodeA)
	require.NoError(t, err)
	require.Contains(t, saved, v.ID)
	assert.Equal(t, key, saved[v.ID].UEFIVarStore.Key, "the running-set write must carry the pointer the stop set")
}

// With predastore unreachable a stop completes, keeps the record on the
// version it booted from and records the obligation; the reaper converges.
func TestS8_UEFIStopCompletesWithPredastoreUnreachableAndTheReaperConverges(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("changed while down"))
	c.objects.setPutErr(errPredastoreDown)
	deferred := uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "deferred")

	start := time.Now()
	a.stop(t, v)
	assert.Less(t, time.Since(start), uefistore.PutTimeout, "the stop must not wait out more than one bounded put")

	assert.Equal(t, c.seedKey, c.stopped(t).UEFIVarStore.Key, "the record keeps the pointer it booted from")
	owed := c.owed(t)
	require.NotNil(t, owed, "the obligation is recorded on the store record")
	assert.Equal(t, nodeA, owed.Node)
	assert.Equal(t, c.seedKey, owed.SeededFrom)
	assert.Equal(t, region("changed while down"), readFile(t, filepath.Join(a.dir(), uefiVarsFileName)), "vars.fd is kept")
	assert.Equal(t, c.seedKey, a.seeded(t))
	assert.Equal(t, deferred+1, uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "deferred"))

	_, err := a.m.NewUEFIWriteBackReaper().Sweep(t.Context())
	require.Error(t, err, "a sweep while predastore is down settles nothing")
	require.NotNil(t, c.owed(t))

	c.objects.setPutErr(nil)
	assert.Equal(t, 1, a.sweep(t))

	rec := c.stopped(t)
	assert.Equal(t, owed.Key, rec.UEFIVarStore.Key, "the reaper puts to the recorded key and points the record at it")
	assert.Equal(t, region("changed while down"), c.region(t, owed.Key))
	assert.Nil(t, c.owed(t))
	assert.Equal(t, owed.Key, a.seeded(t))
	assert.Zero(t, a.sweep(t), "a settled obligation is not retried")
}

// A start on another node before the reaper runs boots the pointer, clears
// the obligation, and the first node's reaper then writes nothing.
func TestUEFIWriteBack_StartElsewhereClearsTheObligationAndBootsThePointer(t *testing.T) {
	c := newWBCluster(t)
	a, b := c.node(t, nodeA), c.node(t, nodeB)
	v := a.newInstance(t, region("lost with node-a"))
	c.objects.setPutErr(errPredastoreDown)
	a.stop(t, v)
	owed := c.owed(t)
	require.NotNil(t, owed)
	c.objects.setPutErr(nil)

	c.stopped(t).LastNode = nodeB
	started := b.restart(t, region("template"))

	assert.Equal(t, region("template"), readFile(t, filepath.Join(b.dir(), uefiVarsFileName)), "node-b boots the pointed object")
	assert.Nil(t, c.owed(t), "the start clears node-a's obligation")
	puts := c.objects.putCount()
	assert.Zero(t, a.sweep(t))
	assert.Equal(t, puts, c.objects.putCount(), "node-a's reaper has nothing left to write")
	assert.Equal(t, c.seedKey, started.UEFIVarStore.Key)
}

// A put that lands while the record write fails is retried at the same key,
// so repeated failures add one object rather than one each.
func TestUEFIWriteBack_RepeatedFailedWriteBacksHoldAtMostFiveObjects(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("v0"))
	type step struct {
		putFails, recordFails bool
	}
	steps := []step{{}, {}, {}, {recordFails: true}, {recordFails: true}, {recordFails: true}, {putFails: true}, {},
		{recordFails: true}, {}, {}, {putFails: true}, {recordFails: true}, {recordFails: true}, {recordFails: true}, {}, {}}
	for i, s := range steps {
		if i > 0 {
			v = a.restart(t, region(fmt.Sprintf("v%d", i)))
		}
		if s.putFails {
			c.objects.setPutErr(errPredastoreDown)
		}
		if s.recordFails {
			c.states.stoppedErr = errors.New("kv unavailable")
		}
		a.stop(t, v)
		c.objects.setPutErr(nil)
		if s.recordFails {
			// The instance is still in the running set, as a failed stopped
			// write leaves it, so the next stop drives it from there.
			c.states.stoppedErr = nil
			require.NoError(t, c.states.fakeStateStore.WriteStoppedInstance(v.ID, v))
			a.m.Delete(v.ID)
		}
		assert.LessOrEqual(t, len(c.keys(t)), 5, "step %d: %v", i, c.keys(t))
		assert.True(t, c.exists(t, c.stopped(t).UEFIVarStore.Key), "step %d: the pointed object exists", i)
	}
	assert.LessOrEqual(t, len(c.keys(t)), 4, "a successful write-back prunes to the newest versions")
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func (c *wbCluster) markDeleting(t *testing.T) {
	t.Helper()
	rec, rev, err := c.records.Get(t.Context(), testUEFIInstance)
	require.NoError(t, err)
	rec.State, rec.WriteBack = uefistore.StateDeleting, nil
	_, err = c.records.CompareAndSet(t.Context(), rec, rev)
	require.NoError(t, err)
}

// A record write that landed without its reply leaves an obligation for a key
// the record already points at; the reaper clears it without another put.
func TestUEFIWriteBackReaper_ClearsAnObligationWhoseRecordWriteLanded(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("landed"))
	c.states.landThenFail = true
	a.stop(t, v)
	c.states.landThenFail = false
	a.m.Delete(v.ID)
	owed := c.owed(t)
	require.NotNil(t, owed)
	require.Equal(t, owed.Key, c.stopped(t).UEFIVarStore.Key)
	landed := uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "landed")
	puts := c.objects.putCount()

	assert.Equal(t, 1, a.sweep(t))

	assert.Nil(t, c.owed(t))
	assert.Equal(t, puts, c.objects.putCount(), "the object is already there")
	assert.Equal(t, owed.Key, a.seeded(t), "a start here reuses the copy the landed key holds")
	assert.Equal(t, landed+1, uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "landed"))
}

// A terminate that marks the store deleting while a stop is writing back owes
// nothing more, so the stop deletes the object it just wrote.
func TestUEFIWriteBack_StoreDeletingAfterThePutDeletesTheObjectJustWritten(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("terminating"))
	var written string
	c.objects.afterPut = func(key string) {
		written = key
		c.markDeleting(t)
	}
	dropped := uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "dropped")

	a.stop(t, v)

	require.NotEmpty(t, written)
	assert.False(t, c.exists(t, written), "the object written under a deleting store is deleted again")
	assert.Equal(t, dropped+1, uefiCounter(t, "mulga.vm.uefi_store.write_backs", "outcome", "dropped"))
	_, err := c.records.LiveWriteBack(t.Context(), testUEFIInstance)
	require.ErrorIs(t, err, uefistore.ErrNotLive)
}

// The reaper's put races a terminate the same way, and the same rule holds:
// the object it put is deleted and the pointer is left alone.
func TestUEFIWriteBackReaper_StoreDeletingDuringTheRetryDeletesItsPut(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("terminating"))
	c.objects.setPutErr(errPredastoreDown)
	a.stop(t, v)
	c.objects.setPutErr(nil)
	owed := c.owed(t)
	require.NotNil(t, owed)
	c.objects.afterPut = func(string) { c.markDeleting(t) }

	assert.Zero(t, a.sweep(t))

	assert.False(t, c.exists(t, owed.Key))
	assert.Equal(t, c.seedKey, c.stopped(t).UEFIVarStore.Key)
}

// A drain-stopped instance stays in the running set, so the reaper moves its
// pointer in memory, where the next running-set write carries it.
func TestUEFIWriteBackReaper_DrainStoppedInstanceGetsItsPointerInMemory(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("drained while down"))
	c.objects.setPutErr(errPredastoreDown)
	require.NoError(t, a.m.StopAll())
	c.objects.setPutErr(nil)
	owed := c.owed(t)
	require.NotNil(t, owed)

	assert.Equal(t, 1, a.sweep(t))

	var key string
	a.m.Inspect(v, func(v *VM) { key = v.UEFIVarStore.Key })
	assert.Equal(t, owed.Key, key)
	assert.Equal(t, region("drained while down"), c.region(t, key))
	saved, _, err := c.states.LoadRunningState(nodeA)
	require.NoError(t, err)
	assert.Equal(t, owed.Key, saved[v.ID].UEFIVarStore.Key, "the reaper's pointer is in the running set it wrote")
	require.NoError(t, a.m.writeRunningState())
	saved, _, err = c.states.LoadRunningState(nodeA)
	require.NoError(t, err)
	assert.Equal(t, owed.Key, saved[v.ID].UEFIVarStore.Key, "and survives the next running-set write")
	assert.Nil(t, c.owed(t))
}

// Once this node has started the instance again from its own copy, the next
// stop writes that copy back, so the older obligation is dropped.
func TestUEFIWriteBackReaper_DropsAnObligationOnceTheInstanceRunsAgain(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("v1"))
	c.states.stoppedErr = assert.AnError
	a.stop(t, v)
	c.states.stoppedErr = nil
	require.NoError(t, c.states.fakeStateStore.WriteStoppedInstance(v.ID, v))
	a.m.Delete(v.ID)
	owed := c.owed(t)
	require.NotNil(t, owed)
	v = a.restart(t, region("v2"))

	assert.Equal(t, 1, a.sweep(t))

	assert.Nil(t, c.owed(t))
	assert.False(t, c.exists(t, owed.Key))
	a.stop(t, v)
	key := c.stopped(t).UEFIVarStore.Key
	assert.NotEqual(t, owed.Key, key)
	assert.Equal(t, region("v2"), c.region(t, key))
}

// A stop that cannot read the instance record cannot check ownership, so it
// puts nothing and leaves the decision to the reaper, which checks it.
func TestUEFIWriteBack_UnreadableInstanceRecordDefersWithoutAPut(t *testing.T) {
	c := newWBCluster(t)
	a := c.node(t, nodeA)
	v := a.newInstance(t, region("unverified"))
	c.pointers.loadErr = errors.New("nats: no responders")
	puts := c.objects.putCount()

	a.stop(t, v)

	assert.Equal(t, puts, c.objects.putCount())
	owed := c.owed(t)
	require.NotNil(t, owed)
	assert.Equal(t, c.seedKey, owed.SeededFrom)

	c.pointers.loadErr = nil
	assert.Equal(t, 1, a.sweep(t))
	assert.Equal(t, owed.Key, c.stopped(t).UEFIVarStore.Key)
	assert.Equal(t, region("unverified"), c.region(t, owed.Key))
}
