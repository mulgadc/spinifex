package dns_test

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/dns"
	"github.com/stretchr/testify/assert"
)

func TestReservedZoneName(t *testing.T) {
	cases := []struct {
		name     string
		reserved bool
	}{
		{"spx3.net.", true},
		{"SPX3.NET", true},
		{"net", true},
		{"compute.internal", true},
		{"services.internal", true},
		{"ap-southeast-2.services.internal", true},
		{"ec2.ap-southeast-2.services.internal.", true},
		{"ip-10-0-0-1.us-west-1.compute.internal.", true},
		{"internal", true},
		{"compute.spx3.net", true},
		{"ec2-1-2-3-4.us-west-1.compute.spx3.net", true},
		{"lb.us-west-1.elb.spx3.net", true},
		{"x.eks.spx3.net", true},
		{"db.rds.spx3.net", true},
		{"ns1.spx3.net", true},
		{"a.ns4.spx3.net", true},
		{"app.spx3.net", false},
		{"nsx.spx3.net", false},
		{"compute.app.spx3.net", false},
		{"example.com", false},
		{"compute.example.com", false},
		{"", true},
	}
	for _, c := range cases {
		assert.Equal(t, c.reserved, dns.ReservedZoneName(c.name, "spx3.net", "", "services.internal"), c.name)
	}
}

func TestReservedZoneNameHonoursTheConfiguredDomains(t *testing.T) {
	assert.True(t, dns.ReservedZoneName("cloud.example.com", "cloud.example.com", "corp.internal", ""))
	assert.True(t, dns.ReservedZoneName("example.com", "cloud.example.com", "corp.internal", ""))
	assert.True(t, dns.ReservedZoneName("host.corp.internal", "cloud.example.com", "corp.internal", ""))
	assert.False(t, dns.ReservedZoneName("spx3.net", "cloud.example.com", "corp.internal", ""))
	assert.False(t, dns.ReservedZoneName("app.spx3.net", "", "", ""))
	assert.False(t, dns.ReservedZoneName("services.internal", "spx3.net", "", ""))
	assert.True(t, dns.ReservedZoneName("svc.spx3.net", "spx3.net", "", "svc.spx3.net"))
	assert.True(t, dns.ReservedZoneName("ec2.us-west-1.svc.spx3.net", "spx3.net", "", "svc.spx3.net"))
}
