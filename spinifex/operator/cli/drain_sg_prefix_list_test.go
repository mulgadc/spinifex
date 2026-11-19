package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ec2/projection"
	ec2vpc "github.com/mulgadc/spinifex/spinifex/domains/ec2/vpc"
	"github.com/mulgadc/spinifex/spinifex/foundation/release/featurelevel"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prefixListFixture is a two-node formation at level 3 with a rollback to 2
// recorded, both vpcds having observed it, idle unless a test changes it.
type prefixListFixture struct {
	drain   *sgPrefixListDrain
	rev     uint64
	rules   int
	proxies map[string]projection.EndpointProxyStatus
	silent  map[string]bool
	out     bytes.Buffer
}

func newPrefixListFixture(t *testing.T) *prefixListFixture {
	t.Helper()
	_, _, js := testutil.StartTestJetStream(t)
	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: "feature-level-test"})
	require.NoError(t, err)
	store := featurelevel.NewStore(kv)
	_, _, err = store.Advance(t.Context(), []featurelevel.Member{{Node: "n1", Level: 3, Current: true}, {Node: "n2", Level: 3, Current: true}})
	require.NoError(t, err)
	_, rev, err := store.StartRollback(t.Context(), 2, featurelevel.Build)
	require.NoError(t, err)

	observed := func(n string) projection.EndpointProxyStatus {
		return projection.EndpointProxyStatus{Node: n, Build: featurelevel.Build, LevelRevision: rev}
	}
	f := &prefixListFixture{rev: rev, silent: map[string]bool{}, proxies: map[string]projection.EndpointProxyStatus{
		"n1": observed("n1"), "n2": observed("n2"),
	}}
	f.drain = &sgPrefixListDrain{
		store: store, nodes: []string{"n1", "n2"}, out: &f.out,
		proxy: func(_ context.Context, node string) (projection.EndpointProxyStatus, error) {
			if f.silent[node] {
				return projection.EndpointProxyStatus{}, errors.New("no reply")
			}
			return f.proxies[node], nil
		},
		rules: func(context.Context) (int, error) { return f.rules, nil },
	}
	return f
}

func TestSGPrefixListDrain_IdleFormationDrains(t *testing.T) {
	f := newPrefixListFixture(t)
	n, err := f.drain.check(t.Context())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// A stored rule, an ACL naming a prefix-list set, or an apply that started
// before a vpcd observed the rollback each hold it on its own.
func TestSGPrefixListDrain_CountsEveryDependent(t *testing.T) {
	cases := map[string]func(f *prefixListFixture){
		"a rule naming a prefix list": func(f *prefixListFixture) { f.rules = 1 },
		"an ACL naming a prefix-list set": func(f *prefixListFixture) {
			for n, st := range f.proxies {
				st.PrefixListACLs = 1
				f.proxies[n] = st
			}
		},
		"an apply from before the rollback": func(f *prefixListFixture) {
			st := f.proxies["n2"]
			st.PrefixListApplies = 1
			f.proxies["n2"] = st
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPrefixListFixture(t)
			mutate(f)
			n, err := f.drain.check(t.Context())
			require.NoError(t, err)
			assert.Equal(t, 1, n)
		})
	}
}

// A vpcd that is silent, behind the rollback write, or too old to report
// prefix-list state, or an unreadable rule count, is not idle.
func TestSGPrefixListDrain_UnreadNodeFails(t *testing.T) {
	cases := map[string]func(f *prefixListFixture){
		"silent vpcd": func(f *prefixListFixture) { f.silent["n2"] = true },
		"vpcd behind the rollback": func(f *prefixListFixture) {
			st := f.proxies["n1"]
			st.LevelRevision = f.rev - 1
			f.proxies["n1"] = st
		},
		"vpcd without the build": func(f *prefixListFixture) {
			f.proxies["n1"] = projection.EndpointProxyStatus{Node: "n1", Revision: f.rev}
		},
		"unreadable security groups": func(f *prefixListFixture) {
			f.drain.rules = func(context.Context) (int, error) { return 0, errors.New("bucket unavailable") }
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPrefixListFixture(t)
			mutate(f)
			_, err := f.drain.check(t.Context())
			require.Error(t, err)
		})
	}
}

func TestSGPrefixListDrain_UnconnectedFails(t *testing.T) {
	_, err := (&sgPrefixListDrain{}).check(t.Context())
	require.Error(t, err)
}

// The count spans accounts and both directions, and ignores other peers.
func TestCountPrefixListRules(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	n, err := ec2vpc.CountPrefixListRules(t.Context(), js)
	require.NoError(t, err)
	assert.Zero(t, n, "an absent bucket holds no rules")

	kv, err := js.CreateKeyValue(t.Context(), jetstream.KeyValueConfig{Bucket: ec2vpc.KVBucketSecurityGroups})
	require.NoError(t, err)
	put := func(key string, rec ec2vpc.SecurityGroupRecord) {
		data, err := json.Marshal(rec)
		require.NoError(t, err)
		_, err = kv.Put(t.Context(), key, data)
		require.NoError(t, err)
	}
	put("111111111111.sg-a", ec2vpc.SecurityGroupRecord{GroupId: "sg-a",
		IngressRules: []ec2vpc.SGRule{{PrefixListId: "pl-6ca54005"}, {CidrIp: "10.0.0.0/8"}},
		EgressRules:  []ec2vpc.SGRule{{PrefixListId: "pl-6ca54005"}},
	})
	put("222222222222.sg-b", ec2vpc.SecurityGroupRecord{GroupId: "sg-b",
		EgressRules: []ec2vpc.SGRule{{PrefixListId: "pl-6ca54005"}, {SourceSG: "sg-a"}},
	})
	n, err = ec2vpc.CountPrefixListRules(t.Context(), js)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}
