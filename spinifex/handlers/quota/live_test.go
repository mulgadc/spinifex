package handlers_quota

import (
	"context"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/awserrors"
	"github.com/mulgadc/spinifex/spinifex/utils"
)

// liveLimits is a representative enabled tier with distinct caps per dimension.
var liveLimits = Limits{Enabled: true, VPCs: 8, Subnets: 16, EIPs: 2, VolumesGiB: 100}

// TestExceeds covers the shared comparison every live dimension runs after its
// Describe* count: under the cap passes, at or over it rejects, and a single
// oversized want is rejected on its own.
func TestExceeds(t *testing.T) {
	tests := []struct {
		name               string
		count, want, limit int
		wantErr            string
	}{
		{"under limit", 7, 1, 8, ""},
		{"at limit rejects", 8, 1, 8, awserrors.ErrorResourceLimitExceeded},
		{"empty fills to limit", 0, 8, 8, ""},
		{"want overshoots limit", 0, 9, 8, awserrors.ErrorResourceLimitExceeded},
		{"count plus want overshoots", 6, 3, 8, awserrors.ErrorResourceLimitExceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := exceeds(tt.count, tt.want, tt.limit)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("exceeds(%d, %d, %d) = %v, want nil", tt.count, tt.want, tt.limit, err)
			case tt.wantErr != "" && (err == nil || err.Error() != tt.wantErr):
				t.Fatalf("exceeds(%d, %d, %d) = %v, want %q", tt.count, tt.want, tt.limit, err, tt.wantErr)
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
		{"vpc disabled", func() error { return disabled.EnforceVPCs(context.Background(), nil, normalAccount, 1) }},
		{"subnet disabled", func() error { return disabled.EnforceSubnets(context.Background(), nil, normalAccount, 1) }},
		{"eip disabled", func() error { return disabled.EnforceEIPs(context.Background(), nil, normalAccount, 1) }},
		{"vpc system account", func() error { return enabled.EnforceVPCs(context.Background(), nil, utils.GlobalAccountID, 1) }},
		{"subnet system account", func() error { return enabled.EnforceSubnets(context.Background(), nil, utils.GlobalAccountID, 1) }},
		{"eip system account", func() error { return enabled.EnforceEIPs(context.Background(), nil, utils.GlobalAccountID, 1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.fn(); err != nil {
				t.Fatalf("exempt path returned %v, want nil", err)
			}
		})
	}
}
