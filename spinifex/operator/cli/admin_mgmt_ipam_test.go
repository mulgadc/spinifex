package cli

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/lambda/execution"
	"github.com/mulgadc/spinifex/spinifex/runtime/compute/vm"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon"
	"github.com/mulgadc/spinifex/spinifex/runtime/daemon/mgmtipstore"
)

const cutoverSubnet = "10.15.8.0/24"

var cutoverNodes = []string{"node-a", "node-b"}

// cutoverCluster is a quiesced cluster's JetStream with the legacy stores.
type cutoverCluster struct {
	nc *nats.Conn
	js jetstream.JetStream
}

func newCutoverCluster(t *testing.T) *cutoverCluster {
	t.Helper()
	_, nc, js := testutil.StartTestJetStream(t)
	for _, b := range []string{daemon.ClusterStateBucket, daemon.InstanceStateBucket} {
		_, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: b})
		require.NoError(t, err)
	}
	return &cutoverCluster{nc: nc, js: js}
}

func (c *cutoverCluster) put(t *testing.T, bucket, key string, v any) {
	t.Helper()
	kv, err := c.js.KeyValue(t.Context(), bucket)
	require.NoError(t, err)
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	_, err = kv.Put(t.Context(), key, raw)
	require.NoError(t, err)
}

func (c *cutoverCluster) legacyKey(t *testing.T, entries ...daemon.MgmtIPEntry) {
	c.put(t, daemon.ClusterStateBucket, daemon.MgmtIPAMLegacyKey(cutoverSubnet),
		daemon.MgmtIPRecord{Subnet: cutoverSubnet, Allocated: entries})
}

func (c *cutoverCluster) instanceRecord(t *testing.T, id, ip, node string) {
	var rec vm.InstanceRecord
	rec.Status.MgmtIP, rec.Status.LastNode = ip, node
	c.put(t, daemon.InstanceStateBucket, daemon.InstanceRecordPrefix+id, rec)
}

func (c *cutoverCluster) lambdaAttempt(t *testing.T, node, attempt, state string) {
	t.Helper()
	_, err := c.js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: execution.KVBucket})
	require.NoError(t, err)
	c.put(t, execution.KVBucket, "node."+node, map[string]any{
		"formatVersion": 1, "seq": 1,
		"attempt": map[string]any{
			"grant": map[string]any{"grantId": "g-1", "seq": 1, "attemptId": attempt, "environmentId": "env-1", "generation": 1, "node": node},
			"state": state,
		},
	})
}

func (c *cutoverCluster) activate(t *testing.T, inv *mgmtInventory, form bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := activateMgmtIPAM(t.Context(), &out, c.js, inv, cutoverNodes, form)
	return out.String(), err
}

func (c *cutoverCluster) seeded(t *testing.T) mgmtipstore.Record {
	t.Helper()
	store, err := mgmtipstore.Bind(t.Context(), c.js, daemon.MgmtIPAMBucket)
	require.NoError(t, err)
	snap, err := store.Load(t.Context(), netipPrefix(t))
	require.NoError(t, err)
	return snap.Record()
}

func (c *cutoverCluster) assertNothingWritten(t *testing.T) {
	t.Helper()
	_, err := c.js.KeyValue(t.Context(), daemon.MgmtIPAMBucket)
	require.ErrorIs(t, err, jetstream.ErrBucketNotFound, "a refused activation writes nothing")
}

func attestedHost(node string, vms ...mgmtInventoryVM) mgmtInventoryHost {
	h := listedHost(node, vms...)
	h.LaunchersQuiesced, h.NATSSyncAlways = true, true
	return h
}

// listedHost has every local list present, explicitly empty when unused.
func listedHost(node string, vms ...mgmtInventoryVM) mgmtInventoryHost {
	if vms == nil {
		vms = []mgmtInventoryVM{}
	}
	return mgmtInventoryHost{Node: node, VMs: &vms, Lambda: &[]mgmtInventoryLambda{}, LambdaTaps: &[]string{}}
}

func withLambda(h mgmtInventoryHost, taps []string, attempts ...mgmtInventoryLambda) mgmtInventoryHost {
	h.Lambda, h.LambdaTaps = &attempts, &taps
	return h
}

func running(id, ip string) mgmtInventoryVM {
	return mgmtInventoryVM{InstanceID: id, MgmtIP: ip, State: inventoryRunning}
}

func stopped(id, ip string, attested bool) mgmtInventoryVM {
	return mgmtInventoryVM{InstanceID: id, MgmtIP: ip, State: inventoryStopped, StoppedAttested: attested}
}

func cutoverInventory(hosts ...mgmtInventoryHost) *mgmtInventory {
	return &mgmtInventory{Schema: mgmtInventorySchema, Subnet: cutoverSubnet, Hosts: hosts}
}

func entry(ip, id, node string) mgmtipstore.Entry {
	return mgmtipstore.Entry{IP: ip, InstanceID: id, Node: node}
}

// The legacy key expired, so the instance records and the inventory are the
// only evidence. Activation seeds them, and a durable allocator then hands out
// an address none of them holds.
func TestMgmtIPAMActivate_MissingLegacyKeySeedsTheAccountedHolders(t *testing.T) {
	c := newCutoverCluster(t)
	c.instanceRecord(t, "i-a", "10.15.8.10", "node-a")

	out, err := c.activate(t, cutoverInventory(
		attestedHost("node-a", running("i-a", "10.15.8.10")),
		attestedHost("node-b", running("i-b", "10.15.8.11")),
	), false)
	require.NoError(t, err, out)

	rec := c.seeded(t)
	assert.Equal(t, mgmtipstore.AuthorityMigrated, rec.Authority)
	assert.Equal(t, []mgmtipstore.Entry{entry("10.15.8.10", "i-a", "node-a"), entry("10.15.8.11", "i-b", "node-b")}, rec.Entries)
	assert.Contains(t, out, "not verified", "the output states what the tool cannot check")

	jsm, err := daemon.NewJetStreamManager(c.nc)
	require.NoError(t, err)
	a, err := daemon.NewMgmtIPAllocator("10.15.8.1")
	require.NoError(t, err)
	require.NoError(t, a.SetMode(daemon.MgmtIPAMDurable))
	a.BindKV(jsm, "node-a")
	ip, err := a.Allocate("i-new")
	require.NoError(t, err)
	assert.Equal(t, "10.15.8.12", ip)
}

func TestMgmtIPAMActivate_UnionOfLegacyKeyRecordsAndLambda(t *testing.T) {
	c := newCutoverCluster(t)
	c.legacyKey(t, daemon.MgmtIPEntry{IP: "10.15.8.10", InstanceID: "i-a", Node: "node-a"},
		daemon.MgmtIPEntry{IP: "10.15.8.12", InstanceID: "i-drained"})
	c.instanceRecord(t, "i-b", "10.15.8.11", "node-b")
	c.lambdaAttempt(t, "node-b", "a1", execution.StateRunning)
	nodeB := withLambda(attestedHost("node-b", running("i-b", "10.15.8.11")), []string{"lma1"},
		mgmtInventoryLambda{AttemptID: "a1", MgmtIP: "10.15.8.13", Tap: "lma1"})

	out, err := c.activate(t, cutoverInventory(
		attestedHost("node-a", running("i-a", "10.15.8.10"), stopped("i-drained", "10.15.8.12", true)),
		nodeB,
	), false)
	require.NoError(t, err, out)

	assert.Equal(t, []mgmtipstore.Entry{
		entry("10.15.8.10", "i-a", "node-a"), entry("10.15.8.11", "i-b", "node-b"),
		entry("10.15.8.12", "i-drained", "node-a"), entry("10.15.8.13", "lambda-a1", "node-b"),
	}, c.seeded(t).Entries)
}

func TestMgmtIPAMActivate_RefusesWithoutWriting(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, c *cutoverCluster)
		inv   *mgmtInventory
		want  string
	}{
		{
			name: "legacy holder no host lists",
			setup: func(t *testing.T, c *cutoverCluster) {
				c.legacyKey(t, daemon.MgmtIPEntry{IP: "10.15.8.20", InstanceID: "i-gone", Node: "node-a"})
			},
			inv:  cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")), attestedHost("node-b")),
			want: "legacy key names i-gone at 10.15.8.20",
		},
		{
			name: "duplicate address",
			inv: cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")),
				attestedHost("node-b", running("i-b", "10.15.8.10"))),
			want: "address 10.15.8.10 is held by both i-a and i-b",
		},
		{
			name:  "record names another node",
			setup: func(t *testing.T, c *cutoverCluster) { c.instanceRecord(t, "i-a", "10.15.8.10", "node-b") },
			inv:   cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")), attestedHost("node-b")),
			want:  "instance record names i-a on node-b but the inventory lists it on node-a",
		},
		{
			name:  "record names another address",
			setup: func(t *testing.T, c *cutoverCluster) { c.instanceRecord(t, "i-a", "10.15.8.30", "node-a") },
			inv:   cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")), attestedHost("node-b")),
			want:  "instance record names i-a at 10.15.8.30 but the inventory lists 10.15.8.10",
		},
		{
			name: "cluster node missing",
			inv:  cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10"))),
			want: "cluster node node-b is missing from the inventory",
		},
		{
			name: "sync not attested",
			inv: cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")),
				func() mgmtInventoryHost { h := listedHost("node-b"); h.LaunchersQuiesced = true; return h }()),
			want: "host node-b: nats_sync_always is not attested",
		},
		{
			name: "quiesce not attested",
			inv: cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")),
				func() mgmtInventoryHost { h := listedHost("node-b"); h.NATSSyncAlways = true; return h }()),
			want: "host node-b: launchers_quiesced is not attested",
		},
		{
			name: "unsettled Lambda attempt with no address",
			setup: func(t *testing.T, c *cutoverCluster) {
				c.lambdaAttempt(t, "node-b", "a1", execution.StateAmbiguous)
			},
			inv:  cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")), attestedHost("node-b")),
			want: "Lambda attempt a1 on node-b is ambiguous and its address is not in the inventory",
		},
		{
			name: "address outside the subnet",
			inv: cutoverInventory(attestedHost("node-a", running("i-a", "10.16.8.10")),
				attestedHost("node-b", running("i-b", "10.15.8.11"))),
			want: `address "10.16.8.10" is not a canonical address in 10.15.8.0/24`,
		},
		{
			name: "Lambda tap no attempt accounts for",
			inv: cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")),
				withLambda(attestedHost("node-b"), []string{"lma9"})),
			want: "host node-b: Lambda tap lma9 belongs to no listed Lambda attempt",
		},
		{
			name: "Lambda attempt whose tap is not listed",
			inv: cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")),
				withLambda(attestedHost("node-b"), []string{},
					mgmtInventoryLambda{AttemptID: "a1", MgmtIP: "10.15.8.13", Tap: "lma1"})),
			want: "host node-b: Lambda attempt a1 names tap lma1, which lambda_taps does not list",
		},
		{
			name: "Lambda attempt naming another attempt's tap",
			inv: cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")),
				withLambda(attestedHost("node-b"), []string{"lma2"},
					mgmtInventoryLambda{AttemptID: "a1", MgmtIP: "10.15.8.13", Tap: "lma2"})),
			want: "host node-b: Lambda attempt a1 names tap lma2, but its tap is lma1",
		},
		{
			name: "no holder without --form",
			inv:  cutoverInventory(attestedHost("node-a"), attestedHost("node-b")),
			want: "rerun with --form only if this is an empty first formation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCutoverCluster(t)
			if tc.setup != nil {
				tc.setup(t, c)
			}

			out, err := c.activate(t, tc.inv, false)

			require.Error(t, err)
			assert.Contains(t, out, tc.want)
			c.assertNothingWritten(t)
		})
	}
}

// A settled Lambda attempt holds no address and needs no inventory entry.
func TestMgmtIPAMActivate_SettledLambdaAttemptIsNotAHolder(t *testing.T) {
	c := newCutoverCluster(t)
	c.lambdaAttempt(t, "node-b", "a1", execution.StateEnded)

	out, err := c.activate(t, cutoverInventory(attestedHost("node-a", running("i-a", "10.15.8.10")), attestedHost("node-b")), false)

	require.NoError(t, err, out)
	assert.Equal(t, []mgmtipstore.Entry{entry("10.15.8.10", "i-a", "node-a")}, c.seeded(t).Entries)
}

func TestMgmtIPAMActivate_FormOnlyForAnAttestedEmptyCluster(t *testing.T) {
	empty := cutoverInventory(attestedHost("node-a"), attestedHost("node-b"))

	t.Run("empty", func(t *testing.T) {
		c := newCutoverCluster(t)
		out, err := c.activate(t, empty, true)
		require.NoError(t, err, out)
		rec := c.seeded(t)
		assert.Equal(t, mgmtipstore.AuthorityFormed, rec.Authority)
		assert.Empty(t, rec.Entries)
	})
	t.Run("legacy key present", func(t *testing.T) {
		c := newCutoverCluster(t)
		c.legacyKey(t)
		out, err := c.activate(t, empty, true)
		require.Error(t, err)
		assert.Contains(t, out, "--form is only for a cluster with no address holder")
		c.assertNothingWritten(t)
	})
	t.Run("inventory lists a guest", func(t *testing.T) {
		c := newCutoverCluster(t)
		out, err := c.activate(t, cutoverInventory(attestedHost("node-a", stopped("i-s", "10.15.8.10", true)), attestedHost("node-b")), true)
		require.Error(t, err)
		assert.Contains(t, out, "--form is only for a cluster with no address holder")
		c.assertNothingWritten(t)
	})
	t.Run("durable record already holds addresses", func(t *testing.T) {
		c := newCutoverCluster(t)
		store, err := mgmtipstore.Create(t.Context(), c.js, daemon.MgmtIPAMBucket, "test")
		require.NoError(t, err)
		_, err = store.Seed(t.Context(), netipPrefix(t), mgmtipstore.AuthorityMigrated, []mgmtipstore.Entry{entry("10.15.8.10", "i-a", "node-a")})
		require.NoError(t, err)
		_, err = c.activate(t, empty, true)
		require.ErrorContains(t, err, "already holds 1 addresses")
		assert.Equal(t, mgmtipstore.AuthorityMigrated, c.seeded(t).Authority)
	})
}

// A stopped guest whose saved address another holder took is not seeded; the
// operator is told its start will be refused.
func TestMgmtIPAMActivate_StoppedGuestWhoseAddressWasReusedIsReported(t *testing.T) {
	c := newCutoverCluster(t)

	out, err := c.activate(t, cutoverInventory(
		attestedHost("node-a", running("i-a", "10.15.8.10"), stopped("i-s", "10.15.8.10", true)),
		attestedHost("node-b"),
	), false)

	require.NoError(t, err, out)
	assert.Contains(t, out, "stopped guest i-s on node-a saved 10.15.8.10, which i-a holds")
	assert.Equal(t, []mgmtipstore.Entry{entry("10.15.8.10", "i-a", "node-a")}, c.seeded(t).Entries)
}

func TestReadMgmtInventory_RefusesUnknownFieldsAndTrailingData(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown field": `{"schema":1,"subnet":"10.15.8.0/24","hosts":[],"extra":1}`,
		"trailing":      `{"schema":1,"subnet":"10.15.8.0/24","hosts":[]}{}`,
		"schema":        `{"schema":2,"subnet":"10.15.8.0/24","hosts":[]}`,
	} {
		path := filepath.Join(dir, "inv.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		_, err := readMgmtInventory(path)
		assert.Error(t, err, name)
	}
}

func readLaunch(t *testing.T, dir, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, id+".json"))
	require.NoError(t, err)
	var rec map[string]any
	require.NoError(t, json.Unmarshal(raw, &rec))
	return rec
}

func TestMgmtIPAMPrepareNode_WritesOnlyAttestedStoppedGuests(t *testing.T) {
	dir := t.TempDir()
	inv := cutoverInventory(attestedHost("node-a",
		running("i-run", "10.15.8.10"),
		stopped("i-attested", "10.15.8.11", true),
		stopped("i-unattested", "10.15.8.12", false),
	))
	var out bytes.Buffer

	require.NoError(t, prepareMgmtNode(&out, inv, "node-a", dir))

	rec := readLaunch(t, dir, "i-attested")
	assert.Equal(t, "exited", rec["state"])
	assert.Equal(t, "node-a", rec["node"])
	assert.Equal(t, "10.15.8.11", rec["mgmt_ip"])
	assert.InDelta(t, 1, rec["generation"], 0)
	for _, id := range []string{"i-run", "i-unattested"} {
		_, err := os.Stat(filepath.Join(dir, id+".json"))
		assert.ErrorIs(t, err, os.ErrNotExist, id)
	}
	assert.Contains(t, out.String(), "i-unattested: HELD")
}

func TestMgmtIPAMPrepareNode_RefusesAMismatchedRecord(t *testing.T) {
	dir := t.TempDir()
	_, err := vm.RecordStoppedMgmtLaunch(dir, "node-a", "i-s", "10.15.8.20")
	require.NoError(t, err)
	var out bytes.Buffer

	err = prepareMgmtNode(&out, cutoverInventory(attestedHost("node-a", stopped("i-s", "10.15.8.11", true))), "node-a", dir)

	require.Error(t, err)
	assert.Equal(t, "10.15.8.20", readLaunch(t, dir, "i-s")["mgmt_ip"])
}

func TestMgmtIPAMPrepareNode_FailsWhileADaemonHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	lock, err := daemon.LockMgmtLaunchDir(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Close() })

	err = prepareMgmtNode(&bytes.Buffer{}, cutoverInventory(attestedHost("node-a", stopped("i-s", "10.15.8.11", true))), "node-a", dir)

	require.ErrorIs(t, err, daemon.ErrMgmtLaunchLocked)
	_, statErr := os.Stat(filepath.Join(dir, "i-s.json"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestMgmtIPAMPrepareNode_RefusesAHostWithoutTheQuiesceAttestation(t *testing.T) {
	dir := t.TempDir()
	inv := cutoverInventory(listedHost("node-a", stopped("i-s", "10.15.8.11", true)))

	require.Error(t, prepareMgmtNode(&bytes.Buffer{}, inv, "node-a", dir))

	_, err := os.Stat(filepath.Join(dir, "i-s.json"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func netipPrefix(t *testing.T) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(cutoverSubnet)
	require.NoError(t, err)
	return p
}

// An omitted local list is a missing inventory and refuses; an explicit empty
// list is an inventory of nothing.
func TestMgmtIPAMActivate_OmittedHostListsRefuseButEmptyListsDoNot(t *testing.T) {
	head := `{"schema":1,"subnet":"10.15.8.0/24","hosts":[` +
		`{"node":"node-a","launchers_quiesced":true,"nats_sync_always":true,` +
		`"vms":[{"instance_id":"i-a","mgmt_ip":"10.15.8.10","state":"running"}],"lambda":[],"lambda_taps":[]},` +
		`{"node":"node-b","launchers_quiesced":true,"nats_sync_always":true`
	cases := map[string]struct {
		body string
		want string
	}{
		"all empty":           {head + `,"vms":[],"lambda":[],"lambda_taps":[]}]}`, ""},
		"vms omitted":         {head + `,"lambda":[],"lambda_taps":[]}]}`, "host node-b: inventory omits vms;"},
		"vms null":            {head + `,"vms":null,"lambda":[],"lambda_taps":[]}]}`, "host node-b: inventory omits vms;"},
		"lambda omitted":      {head + `,"vms":[],"lambda_taps":[]}]}`, "host node-b: inventory omits lambda;"},
		"lambda_taps omitted": {head + `,"vms":[],"lambda":[]}]}`, "host node-b: inventory omits lambda_taps;"},
		"all omitted":         {head + `}]}`, "host node-b: inventory omits vms, lambda, lambda_taps;"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inv.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			inv, err := readMgmtInventory(path)
			require.NoError(t, err)
			c := newCutoverCluster(t)

			out, err := c.activate(t, inv, false)

			if tc.want == "" {
				require.NoError(t, err, out)
				assert.Equal(t, []mgmtipstore.Entry{entry("10.15.8.10", "i-a", "node-a")}, c.seeded(t).Entries)
				return
			}
			require.Error(t, err)
			assert.Contains(t, out, tc.want)
			c.assertNothingWritten(t)
		})
	}
}

func TestMgmtIPAMPrepareNode_RefusesAHostWithAnOmittedList(t *testing.T) {
	dir := t.TempDir()
	h := attestedHost("node-a", stopped("i-s", "10.15.8.11", true))
	h.LambdaTaps = nil

	err := prepareMgmtNode(&bytes.Buffer{}, cutoverInventory(h), "node-a", dir)

	require.ErrorContains(t, err, "inventory omits lambda_taps")
	_, statErr := os.Stat(filepath.Join(dir, "i-s.json"))
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
