package handlers_ec2_instance

import (
	"testing"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/mulgadc/spinifex/spinifex/config"
	handlers_dns "github.com/mulgadc/spinifex/spinifex/handlers/dns"
	"github.com/mulgadc/spinifex/spinifex/vm"
)

// publishDNS must be a safe no-op when northstar is not configured (no base
// domain) and must tolerate a nil NATS connection while building changes.
func TestPublishDNS(t *testing.T) {
	inst := &vm.VM{
		PublicIP: "1.2.3.4",
		Instance: &ec2.Instance{PrivateIpAddress: aws.String("172.31.26.216")},
	}

	// Disabled: no base domain → no panic, no publish attempt.
	disabled := &InstanceServiceImpl{config: &config.Config{Region: "ap-southeast-2"}}
	disabled.publishDNS("123456789012", handlers_dns.ActionUpsert, []*vm.VM{inst})

	// Enabled base domain but nil NATS conn: builds changes, best-effort publish
	// is a no-op for a nil connection (must not panic).
	enabled := &InstanceServiceImpl{
		config:        &config.Config{Region: "ap-southeast-2"},
		dnsBaseDomain: "spx3.net",
	}
	enabled.publishDNS("123456789012", handlers_dns.ActionUpsert, []*vm.VM{inst})
	enabled.publishDNS("123456789012", handlers_dns.ActionDelete, []*vm.VM{inst})
	enabled.publishDNS("123456789012", handlers_dns.ActionUpsert, nil)
}
