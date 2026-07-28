package handlers_acm

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// hostsZoneFake returns a NorthstarHostsZone func that reports true only for
// the exact domains listed — no suffix matching, so a test that never lists a
// parent zone can never accidentally pass through it.
func hostsZoneFake(hosted ...string) func(string) bool {
	set := make(map[string]bool, len(hosted))
	for _, h := range hosted {
		set[h] = true
	}
	return func(domain string) bool { return set[domain] }
}

// TestDeriveValidationMode exercises the precedence directly against
// deriveValidationMode, independent of RequestCertificate's surrounding
// plumbing.
func TestDeriveValidationMode(t *testing.T) {
	tenantCA := &fakeCertAuthority{permitted: map[string]bool{
		"private.example.com": true,
	}}

	cases := []struct {
		name                  string
		tenantCA              CertAuthority
		dnsProviderConfigured bool
		northstarHostsZone    func(string) bool
		domains               []string
		want                  string
	}{
		{
			name:    "no signals at all falls back to PRIVATE_CA",
			domains: []string{"unknown.example.com"},
			want:    ValidationModePrivateCA,
		},
		{
			name:               "domain outside every hosted zone does not select MANUAL_TXT",
			northstarHostsZone: hostsZoneFake("other.example.org"),
			domains:            []string{"unrelated.example.com"},
			want:               ValidationModePrivateCA,
		},
		{
			name:               "domain inside a hosted zone selects MANUAL_TXT when nothing else covers it",
			northstarHostsZone: hostsZoneFake("lab.example.com"),
			domains:            []string{"lab.example.com"},
			want:               ValidationModeManualTXT,
		},
		{
			name:               "tenant CA coverage wins over a northstar-hosted zone (demo-critical)",
			tenantCA:           tenantCA,
			northstarHostsZone: hostsZoneFake("private.example.com"),
			domains:            []string{"private.example.com"},
			want:               ValidationModePrivateCA,
		},
		{
			name: "mixed SAN coverage under the tenant CA falls through instead of selecting PRIVATE_CA via rule 1",
			// tenantCA only permits "private.example.com", not the SAN, so rule 1
			// must not fire; northstar hosts both domains, so the correct result
			// (falling through to rule 3) is MANUAL_TXT, not PRIVATE_CA.
			tenantCA:           tenantCA,
			northstarHostsZone: hostsZoneFake("private.example.com", "other.example.com"),
			domains:            []string{"private.example.com", "other.example.com"},
			want:               ValidationModeManualTXT,
		},
		{
			name: "mixed SAN coverage under northstar does not select MANUAL_TXT",
			// Only one of the two domains is hosted; a buggy "any" check would
			// select MANUAL_TXT here instead of falling through to PRIVATE_CA.
			northstarHostsZone: hostsZoneFake("hosted.example.com"),
			domains:            []string{"hosted.example.com", "unhosted.example.com"},
			want:               ValidationModePrivateCA,
		},
		{
			name:                  "dnsProviderConfigured still wins over a northstar-hosted zone when no tenant CA covers the domain",
			dnsProviderConfigured: true,
			northstarHostsZone:    hostsZoneFake("provider.example.com"),
			domains:               []string{"provider.example.com"},
			want:                  ValidationModeProviderAPI,
		},
		{
			name:                  "dnsProviderConfigured wins even when the tenant CA is wired but does not cover the domain",
			tenantCA:              tenantCA,
			dnsProviderConfigured: true,
			domains:               []string{"provider-only.example.com"},
			want:                  ValidationModeProviderAPI,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &ACMServiceImpl{
				TenantCA:              tc.tenantCA,
				dnsProviderConfigured: tc.dnsProviderConfigured,
				NorthstarHostsZone:    tc.northstarHostsZone,
			}
			assert.Equal(t, tc.want, svc.deriveValidationMode(tc.domains))
		})
	}
}
