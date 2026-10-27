//test:in-package — these drive reconcileOnce against a real Writer over the
//fake S3, which needs the unexported Reconciler fields and the writer's own
//resolved S3 config.

package dns

import (
	"testing"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reconcilerFor pairs a Reconciler with the writer that applies its batches,
// sharing one bucket. nc stays nil so a pass skips the election and runs.
func reconcilerFor(w *Writer, desired func() DesiredSet) *Reconciler {
	return &Reconciler{
		enabled:        true,
		baseDomain:     w.baseDomain,
		internalDomain: "compute.internal",
		s3cfg:          w.s3cfg,
		writer:         w,
		desired:        desired,
	}
}

// labelsOfType lists the zone-relative labels carried by records of one type,
// read back through the same parser northstar uses.
func labelsOfType(t *testing.T, s3cfg *nsconfig.S3Config, zone string, rtype uint16) []string {
	t.Helper()
	cfg, exists, err := nsconfig.ReadZoneRaw(s3cfg, zone)
	require.NoError(t, err)
	require.True(t, exists, "zone %s should exist", zone)
	var out []string
	for _, rec := range cfg.Records {
		if rec.Type == rtype {
			out = append(out, rec.Domain)
		}
	}
	return out
}

// The pass applies its own batch. There is no NATS connection here, so nothing
// could carry a ChangeBatch to a peer: if the record reaches the zone object,
// the leader wrote it.
func TestReconcileOnce_WritesTheZoneWithoutPublishing(t *testing.T) {
	w, objects := newTestWriter(t)
	r := reconcilerFor(w, func() DesiredSet {
		return DesiredSet{Changes: []Change{upsert("lb-1.elb."+testBase, "10.200.1.9")}}
	})

	r.reconcileOnce(t.Context())

	assert.Contains(t, objects[testBase+".toml"], `address = "10.200.1.9"`,
		"a pass with no transport still has to reach the zone object, or it published instead of writing")
}

// The desired set holds no apex, SOA, NS or glue record — the writer
// materialises those from cluster config. A pass that pruned everything absent
// from the desired set would take the NS set with it, which is an outage for the
// zone rather than a missing address.
func TestReconcileOnce_StructuralRecordsSurviveAPrune(t *testing.T) {
	w, objects := newTestWriter(t)
	objects[testBase+".toml"] = `version = 1.0
[domain]
domain = "spx3.net"
active = true
soa = "ns1.spx3.net."
[defaults]
ttl = 300
type = 1
class = 1
[[records]]
domain = ""
type = 2
address = "ns1.spx3.net."
[[records]]
domain = "ns1."
type = 1
address = "10.0.0.1"
[[records]]
domain = "live.elb."
type = 1
address = "10.200.1.9"
[[records]]
domain = "stale.elb."
type = 1
address = "10.200.1.99"
`

	r := reconcilerFor(w, func() DesiredSet {
		return DesiredSet{
			Changes:  []Change{upsert("live.elb."+testBase, "10.200.1.9")},
			Prunable: PruneScope{ELB: true, EKS: true, RDS: true, EC2: true},
		}
	})

	r.reconcileOnce(t.Context())

	body := objects[testBase+".toml"]
	assert.NotContains(t, body, `address = "10.200.1.99"`, "the stale ELB record is the one thing a pass may delete")
	assert.Contains(t, body, `address = "10.200.1.9"`, "the live ELB record must survive")
	assert.Contains(t, body, `soa = "ns1.spx3.net."`, "the SOA is not in the desired set and must survive")

	assert.Equal(t, []string{""}, labelsOfType(t, w.s3cfg, testBase, nsconfig.TypeNS),
		"the apex NS set is not in the desired set and must survive a pass")
	assert.Contains(t, labelsOfType(t, w.s3cfg, testBase, nsconfig.TypeA), "ns1.",
		"nameserver glue is not in the desired set and must survive a pass")
}

// A zone a pass creates has to arrive complete. The private zone does not exist
// until an instance lands in it, so the first pass to write one materialises it,
// and a zone materialised without its apex and NS set does not resolve at all.
func TestReconcileOnce_MaterialisedZoneCarriesApexAndNameservers(t *testing.T) {
	w, objects := newMultiNodeTestWriter(t)
	r := reconcilerFor(w, func() DesiredSet {
		return DesiredSet{Changes: []Change{{
			Action: ActionUpsert, Zone: "compute.internal",
			Name: "ip-10-20-0-5.ap-southeast-2.compute.internal", Type: "A", Value: "10.20.0.5",
		}}}
	})

	r.reconcileOnce(t.Context())

	body := objects["compute.internal.toml"]
	require.NotEmpty(t, body, "the pass must materialise the zone it writes into")

	// Both cluster nodes, at the apex: a materialised zone with no NS set is a
	// zone that does not resolve.
	assert.Equal(t, []string{"", ""}, labelsOfType(t, w.s3cfg, "compute.internal", nsconfig.TypeNS),
		"every NS record belongs at the apex, one per cluster nameserver")
	assert.Contains(t, body, `address = "ns1.compute.internal."`)
	assert.Contains(t, body, `address = "ns2.compute.internal."`)

	cfg, _, err := nsconfig.ReadZoneRaw(w.s3cfg, "compute.internal")
	require.NoError(t, err)
	assert.Equal(t, "compute.internal", cfg.Domain.Domain, "the apex names the zone")
	assert.Equal(t, "ns1.compute.internal.", cfg.Domain.SOA, "a zone with no SOA is not answerable")
	assert.Subset(t, labelsOfType(t, w.s3cfg, "compute.internal", nsconfig.TypeA), []string{"ns1.", "ns2."},
		"the NS set needs its glue or the delegation is unresolvable")
}

// nameserverDesired is the desired set the daemon builds for the base zone's
// nameservers from a cluster of n northstar nodes.
func nameserverDesired(n int) func() DesiredSet {
	return func() DesiredSet {
		published := PublishedNameservers(&config.ClusterConfig{Node: "node01", Nodes: northstarNodes(n)})
		ch := BaseZoneNameserverChanges(testBase, published)
		return DesiredSet{Changes: ch, Prunable: PruneScope{Nameservers: len(ch) > 0}}
	}
}

func recordsOfType(t *testing.T, s3cfg *nsconfig.S3Config, rtype uint16) map[string]string {
	t.Helper()
	cfg, exists, err := nsconfig.ReadZoneRaw(s3cfg, testBase)
	require.NoError(t, err)
	require.True(t, exists)
	out := map[string]string{}
	for _, rec := range cfg.Records {
		if rec.Type == rtype {
			out[rec.Domain+" "+rec.Address] = rec.Address
		}
	}
	return out
}

// The base zone is seeded once, by whichever node formed the cluster first, so
// the apex NS set and glue follow the cluster only because each pass holds them
// to the published assignment: a join adds nsK up to four, a fifth node changes
// nothing, and a node that leaves takes its glue with it.
func TestReconcileOnce_BaseZoneNameserversFollowTheCluster(t *testing.T) {
	w, objects := newTestWriter(t)
	objects[testBase+".toml"] = `version = 1.0
[domain]
domain = "spx3.net"
active = true
soa = "ns1.spx3.net."
[defaults]
ttl = 300
type = 1
class = 1
[[records]]
domain = ""
type = 2
address = "ns1.spx3.net."
[[records]]
domain = "ns1."
type = 1
address = "10.0.0.1"
[[records]]
domain = "www."
type = 1
address = "10.9.9.9"
`
	nodes := 3
	r := reconcilerFor(w, func() DesiredSet { return nameserverDesired(nodes)() })

	r.reconcileOnce(t.Context())
	assert.Equal(t, map[string]string{
		" ns1.spx3.net.": "ns1.spx3.net.", " ns2.spx3.net.": "ns2.spx3.net.", " ns3.spx3.net.": "ns3.spx3.net.",
	}, recordsOfType(t, w.s3cfg, nsconfig.TypeNS))
	assert.Equal(t, map[string]string{
		"ns1. 10.0.0.1": "10.0.0.1", "ns2. 10.0.0.2": "10.0.0.2", "ns3. 10.0.0.3": "10.0.0.3", "www. 10.9.9.9": "10.9.9.9",
	}, recordsOfType(t, w.s3cfg, nsconfig.TypeA))

	idle := objects[testBase+".toml"]
	r.reconcileOnce(t.Context())
	assert.Equal(t, idle, objects[testBase+".toml"], "an unchanged cluster rewrites nothing")

	nodes = 5
	r.reconcileOnce(t.Context())
	assert.Len(t, recordsOfType(t, w.s3cfg, nsconfig.TypeNS), 4, "a fifth node is not published")
	assert.Contains(t, recordsOfType(t, w.s3cfg, nsconfig.TypeA), "ns4. 10.0.0.4")
	assert.NotContains(t, recordsOfType(t, w.s3cfg, nsconfig.TypeA), "ns5. 10.0.0.5")

	nodes = 2
	r.reconcileOnce(t.Context())
	assert.Len(t, recordsOfType(t, w.s3cfg, nsconfig.TypeNS), 2)
	assert.Equal(t, map[string]string{
		"ns1. 10.0.0.1": "10.0.0.1", "ns2. 10.0.0.2": "10.0.0.2", "www. 10.9.9.9": "10.9.9.9",
	}, recordsOfType(t, w.s3cfg, nsconfig.TypeA), "glue for a departed nameserver is removed and nothing else is")
}

// A topology that publishes no nameserver, such as a loopback-only dev node,
// leaves the seeded set alone rather than emptying it.
func TestReconcileOnce_NoPublishedNameserversKeepsTheSeed(t *testing.T) {
	assert.Empty(t, BaseZoneNameserverChanges(testBase, nil))
	prune := prunableFor(PruneScope{})
	assert.False(t, prune(testBase, "ns1."), "glue is prunable only with nameserver authority")
	authorised := prunableFor(PruneScope{Nameservers: true})
	assert.True(t, authorised(testBase, "ns3."))
	assert.False(t, authorised(testBase, ""), "the apex is never pruned")
	assert.False(t, authorised(testBase, "nsfoo."))
	assert.False(t, authorised(testBase, "ns3.app."))
}
