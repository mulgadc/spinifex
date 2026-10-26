//test:in-package — newRoute53Service is an unexported Daemon method and reads
//the unexported config and clusterConfig fields.

package daemon

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/route53"
	testutil "github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/bootstrap/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRoute53ServiceDelegatesToThePublishedNameservers(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	ns := config.NorthstarConfig{ConfigPath: "/etc/spinifex/northstar.toml", DefaultDomain: "spx3.net"}
	nodes := map[string]config.Config{
		"node1": {AdvertiseIP: "10.0.0.1", Northstar: ns},
		"node2": {AdvertiseIP: "10.0.0.2", Northstar: ns},
	}
	local := nodes["node1"]
	d := &Daemon{config: &local, clusterConfig: &config.ClusterConfig{Node: "node1", Nodes: nodes}}
	svc := d.newRoute53Service(js)

	out, err := svc.CreateHostedZone(context.Background(), &route53.CreateHostedZoneInput{
		Name: aws.String("example.com"), CallerReference: aws.String("ref-1"),
	}, "000000000001")
	require.NoError(t, err)
	assert.Equal(t, []string{"ns1.spx3.net", "ns2.spx3.net"}, aws.StringValueSlice(out.DelegationSet.NameServers))

	_, err = svc.CreateHostedZone(context.Background(), &route53.CreateHostedZoneInput{
		Name: aws.String("lb.elb.spx3.net"), CallerReference: aws.String("ref-2"),
	}, "000000000001")
	assert.ErrorContains(t, err, "InvalidDomainName")
}

func TestRoute53ServiceRefusesZonesWithoutABaseDomain(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	local := config.Config{AdvertiseIP: "10.0.0.1"}
	d := &Daemon{config: &local, clusterConfig: &config.ClusterConfig{Node: "node1", Nodes: map[string]config.Config{"node1": local}}}

	_, err := d.newRoute53Service(js).CreateHostedZone(context.Background(), &route53.CreateHostedZoneInput{
		Name: aws.String("example.com"), CallerReference: aws.String("ref-1"),
	}, "000000000001")
	assert.ErrorContains(t, err, "no nameservers")
}
