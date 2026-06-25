package handlers_quota

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// liveLimits is a representative enabled tier with distinct caps per dimension
// so each case confirms EnforceLive selects the matching limit.
var liveLimits = Limits{Enabled: true, VPCs: 8, Subnets: 16, EIPs: 2, VolumesGiB: 100}

func TestEnforceLive(t *testing.T) {
	s := New(liveLimits, nil)
	tests := []struct {
		name         string
		resourceType string
		count, want  int
		wantErr      string
	}{
		{"vpc under limit", ResourceVPC, 7, 1, ""},
		{"vpc at limit rejects", ResourceVPC, 8, 1, awserrors.ErrorResourceLimitExceeded},
		{"vpc empty fills to limit", ResourceVPC, 0, 8, ""},
		{"vpc want overshoots limit", ResourceVPC, 0, 9, awserrors.ErrorResourceLimitExceeded},
		{"vpc count plus want overshoots", ResourceVPC, 6, 3, awserrors.ErrorResourceLimitExceeded},
		{"subnet under limit", ResourceSubnet, 15, 1, ""},
		{"subnet at limit rejects", ResourceSubnet, 16, 1, awserrors.ErrorResourceLimitExceeded},
		{"eip under limit", ResourceEIP, 1, 1, ""},
		{"eip at limit rejects", ResourceEIP, 2, 1, awserrors.ErrorResourceLimitExceeded},
		{"unknown type errors", "subnetwork", 0, 1, awserrors.ErrorServerInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.EnforceLive(tt.resourceType, tt.count, tt.want)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("EnforceLive(%q, %d, %d) = %v, want nil", tt.resourceType, tt.count, tt.want, err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("EnforceLive(%q, %d, %d) = %v, want %q", tt.resourceType, tt.count, tt.want, err, tt.wantErr)
			}
		})
	}
}

// Exempt callers must short-circuit before the Describe* round trip, so the
// per-dimension methods return nil even with a nil NATS connection (which would
// otherwise panic on describe). This covers both the disabled-config and the
// system-account exemptions for all three live dimensions.
func TestEnforceLiveExemptShortCircuits(t *testing.T) {
	const normalAccount = "123456789012"
	disabled := New(Limits{Enabled: false}, nil)
	enabled := New(liveLimits, nil)
	cases := []struct {
		name string
		fn   func() error
	}{
		{"vpc disabled", func() error { return disabled.EnforceVPCs(nil, normalAccount, 1) }},
		{"subnet disabled", func() error { return disabled.EnforceSubnets(nil, normalAccount, 1) }},
		{"eip disabled", func() error { return disabled.EnforceEIPs(nil, normalAccount, 1) }},
		{"vpc system account", func() error { return enabled.EnforceVPCs(nil, utils.GlobalAccountID, 1) }},
		{"subnet system account", func() error { return enabled.EnforceSubnets(nil, utils.GlobalAccountID, 1) }},
		{"eip system account", func() error { return enabled.EnforceEIPs(nil, utils.GlobalAccountID, 1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err != nil {
				t.Fatalf("exempt path returned %v, want nil", err)
			}
		})
	}
}
