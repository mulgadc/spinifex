package handlers_elbv2

import (
	"testing"

	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLBFrontendIP(t *testing.T) {
	// internet-facing → the AZ-0 public IP.
	inet := &LoadBalancerRecord{
		Scheme:     SchemeInternetFacing,
		VPCIP:      "10.0.0.5",
		AvailZones: []AvailZoneInfo{{PublicIP: "203.0.113.7"}},
	}
	assert.Equal(t, "203.0.113.7", lbFrontendIP(inet))

	// internet-facing before the public IP is allocated → VPC IP fallback.
	inetNoPub := &LoadBalancerRecord{
		Scheme:     SchemeInternetFacing,
		VPCIP:      "10.0.0.5",
		AvailZones: []AvailZoneInfo{{}},
	}
	assert.Equal(t, "10.0.0.5", lbFrontendIP(inetNoPub))

	// internal → VPC IP even if a public IP is somehow present.
	internal := &LoadBalancerRecord{
		Scheme:     SchemeInternal,
		VPCIP:      "10.0.0.9",
		AvailZones: []AvailZoneInfo{{PublicIP: "203.0.113.7"}},
	}
	assert.Equal(t, "10.0.0.9", lbFrontendIP(internal))
}

func TestPublishLBDNS_NoopWhenDisabled(t *testing.T) {
	rec := &LoadBalancerRecord{
		Scheme:    SchemeInternal,
		DNSName:   "internal-web-abc.ap-southeast-2.elb.spx3.net",
		VPCIP:     "10.0.0.1",
		AccountID: "000000000000",
	}
	// No base domain → no-op; must not panic on the nil NATS connection.
	(&ELBv2ServiceImpl{}).publishLBDNS(rec, handlers_dns.ActionUpsert)
	// Base domain set but nil NATS conn → PublishChangesBestEffort tolerates it.
	(&ELBv2ServiceImpl{dnsBaseDomain: "spx3.net"}).publishLBDNS(rec, handlers_dns.ActionDelete)
}

func TestDesiredDNSChanges_IncludesEndpointReadyProvisioningLoadBalancers(t *testing.T) {
	svc := setupTestService(t)
	svc.dnsBaseDomain = "spx3.net"

	provisioning := newTestLB("provisioning123", "provisioning")
	provisioning.State = StateProvisioning
	provisioning.DNSName = "provisioning-123.ap-southeast-2.elb.spx3.net"
	provisioning.VPCIP = "10.0.0.10"
	require.NoError(t, svc.store.PutLoadBalancer(provisioning))

	provisioningPublic := newTestLB("public12345678", "public")
	provisioningPublic.State = StateProvisioning
	provisioningPublic.Scheme = SchemeInternetFacing
	provisioningPublic.DNSName = "public-123.ap-southeast-2.elb.spx3.net"
	provisioningPublic.VPCIP = "10.0.0.11"
	provisioningPublic.AvailZones = []AvailZoneInfo{{PublicIP: "203.0.113.11"}}
	require.NoError(t, svc.store.PutLoadBalancer(provisioningPublic))

	active := newTestLB("active12345678", "active")
	active.State = StateActive
	active.DNSName = "active-123.ap-southeast-2.elb.spx3.net"
	active.VPCIP = "10.0.0.12"
	require.NoError(t, svc.store.PutLoadBalancer(active))

	failed := newTestLB("failed12345678", "failed")
	failed.State = StateFailed
	failed.DNSName = "failed-123.ap-southeast-2.elb.spx3.net"
	failed.VPCIP = "10.0.0.13"
	require.NoError(t, svc.store.PutLoadBalancer(failed))

	changes, authoritative := svc.DesiredDNSChanges()
	require.True(t, authoritative)
	require.Len(t, changes, 3)
	got := make(map[string]string, len(changes))
	for _, change := range changes {
		got[change.Name] = change.Value
	}
	assert.Equal(t, map[string]string{
		provisioning.DNSName:       provisioning.VPCIP,
		provisioningPublic.DNSName: "203.0.113.11",
		active.DNSName:             active.VPCIP,
	}, got)
}
