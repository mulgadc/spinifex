//test:in-package — prunable, computeBatch, zoneRecord and the label prefixes are
//all unexported, and what these tests pin is which records a pass is willing to
//delete rather than anything the package exports.

package dns

import (
	"fmt"
	"testing"

	nsconfig "github.com/mulgadc/northstar/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPrivate = "compute.internal"

// prunableForZones mirrors prunableFor with a private zone configured, which is
// what makes instance records reachable at all.
func prunableForZones(scope PruneScope) func(zone, label string) bool {
	r := &Reconciler{baseDomain: testBase, internalDomain: testPrivate}
	return r.prunable(scope)
}

// zoneTOML renders a minimal parseable zone carrying an apex NS plus the given
// A records, so a test can seed one into the fake S3 and read it back.
func zoneTOML(domain string, records map[string]string) string {
	out := fmt.Sprintf("version = 1.0\n[domain]\ndomain = %q\nactive = true\nsoa = \"ns1.%s.\"\n"+
		"[defaults]\nttl = 300\ntype = 1\nclass = 1\n"+
		"[[records]]\ndomain = \"\"\ntype = 2\naddress = \"ns1.%s.\"\n", domain, domain, domain)
	for label, addr := range records {
		out += fmt.Sprintf("[[records]]\ndomain = %q\ntype = 1\naddress = %q\n", label, addr)
	}
	return out
}

// An instance record is prunable in both of the zones it lands in, and only
// with the authority that says the instance view spanned every node. Without
// that authority pruning would delete the records of every instance running
// somewhere else, which is what the vmMgr-backed desired set could not avoid.
func TestPrunable_InstanceRecordsNeedClusterWideAuthority(t *testing.T) {
	public := "ec2-3-3-3-3.ap-southeast-2.compute"
	private := "ip-172-31-0-6.ap-southeast-2"

	withAuthority := prunableForZones(PruneScope{EC2: true})
	assert.True(t, withAuthority(testBase, public), "a public instance record in the base zone")
	assert.True(t, withAuthority(testPrivate, private), "a private instance record in the private zone")

	without := prunableForZones(PruneScope{ELB: true, EKS: true, RDS: true})
	assert.False(t, without(testBase, public),
		"without EC2 authority the instance view may be one node's, so nothing may be pruned")
	assert.False(t, without(testPrivate, private),
		"authority for the other classes does not extend to instances")
}

// The private zone holds instance records and structural records, nothing else.
// Pruning it on "absent from the desired set" alone would take the NS set with
// it, which is a zone outage rather than a stale address.
func TestPrunable_PrivateZoneStructuralRecordsSurvive(t *testing.T) {
	prune := prunableForZones(PruneScope{EC2: true, ELB: true, EKS: true, RDS: true})
	for _, label := range []string{"", "ns1", "soa"} {
		assert.False(t, prune(testPrivate, label),
			"structural record %q in the private zone must never be pruned", label)
	}
}

// An address that no running instance holds is stale in both zones at once: the
// record name is derived from the IP, so a reallocated address otherwise keeps
// resolving to whatever held it before.
func TestComputeConverge_PrunesAStaleInstanceRecordInBothZones(t *testing.T) {
	desired := []Change{
		upsert("ec2-3-3-3-3.ap-southeast-2.compute."+testBase, "3.3.3.3"),
		{Action: ActionUpsert, Zone: testPrivate,
			Name: "ip-172-31-0-6.ap-southeast-2." + testPrivate, Type: "A", Value: "172.31.0.6"},
	}
	// Stored labels are zone-relative and keep their trailing dot, which is what
	// relativeLabel produces and therefore what a desired record is matched on.
	existing := map[string][]zoneRecord{
		testBase: {
			existingA("ec2-3-3-3-3.ap-southeast-2.compute.", "3.3.3.3"),
			existingA("ec2-9-9-9-9.ap-southeast-2.compute.", "9.9.9.9"),
			{label: "", rtype: nsconfig.TypeNS, value: "ns1." + testBase + "."},
		},
		testPrivate: {
			existingA("ip-172-31-0-6.ap-southeast-2.", "172.31.0.6"),
			existingA("ip-172-31-0-99.ap-southeast-2.", "172.31.0.99"),
			{label: "", rtype: nsconfig.TypeNS, value: "ns1." + testPrivate + "."},
		},
	}

	batch, err := computeConverge(desired, existing, prunableForZones(PruneScope{EC2: true}))
	require.NoError(t, err)

	deletes := deletesOf(batch)
	names := make([]string, 0, len(deletes))
	for _, d := range deletes {
		names = append(names, d.Name)
	}
	assert.ElementsMatch(t, []string{
		"ec2-9-9-9-9.ap-southeast-2.compute." + testBase,
		"ip-172-31-0-99.ap-southeast-2." + testPrivate,
	}, names, "both zones prune the address no running instance holds, and nothing else")
}

// End to end through the real zone read: the private zone has to be one of the
// zones a pass reads, or its stale records are never even considered. Whether
// it is read when no pass could act on it is an S3 GET, not a behaviour, so it
// is not asserted here.
func TestComputeBatch_PrunesBothZonesUnderEC2Authority(t *testing.T) {
	endpoint, objects := fakeS3(t, "northstar")
	objects[testBase+".toml"] = zoneTOML(testBase, map[string]string{
		"ec2-3-3-3-3.ap-southeast-2.compute.": "3.3.3.3",
		"ec2-9-9-9-9.ap-southeast-2.compute.": "9.9.9.9",
	})
	objects[testPrivate+".toml"] = zoneTOML(testPrivate, map[string]string{
		"ip-172-31-0-6.ap-southeast-2.":  "172.31.0.6",
		"ip-172-31-0-99.ap-southeast-2.": "172.31.0.99",
	})

	// One instance is still running, so its two records must survive both passes.
	live := []Change{
		upsert("ec2-3-3-3-3.ap-southeast-2.compute."+testBase, "3.3.3.3"),
		{Action: ActionUpsert, Zone: testPrivate,
			Name: "ip-172-31-0-6.ap-southeast-2." + testPrivate, Type: "A", Value: "172.31.0.6"},
	}
	scope := PruneScope{}
	r := &Reconciler{
		enabled:        true,
		baseDomain:     testBase,
		internalDomain: testPrivate,
		s3cfg: &nsconfig.S3Config{
			Endpoint: endpoint, Bucket: "northstar", Region: "us-east-1",
			AccessKey: "SYSTEM", SecretKey: "SYSTEMSECRET",
		},
		desired: func() DesiredSet { return DesiredSet{Changes: live, Prunable: scope} },
	}

	batch, err := r.computeBatch()
	require.NoError(t, err)
	assert.Empty(t, deletesOf(batch), "no EC2 authority prunes nothing in either zone")

	scope = PruneScope{EC2: true}
	batch, err = r.computeBatch()
	require.NoError(t, err)
	deletes := deletesOf(batch)
	require.Len(t, deletes, 2, "with authority both zones are read and both stale records prune")
	assert.ElementsMatch(t, []string{
		"ec2-9-9-9-9.ap-southeast-2.compute." + testBase,
		"ip-172-31-0-99.ap-southeast-2." + testPrivate,
	}, []string{deletes[0].Name, deletes[1].Name})
}
