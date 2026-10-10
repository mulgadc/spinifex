//test:in-package — these drive reconcileOnce against a real Writer over the
//fake S3, which needs the unexported Reconciler fields and the writer's own
//resolved S3 config.

package dns

import (
	"testing"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
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
