package handlers_elbv2

import (
	"testing"

	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/stretchr/testify/assert"
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
