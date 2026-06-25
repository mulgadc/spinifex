package handlers_quota

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/utils"
	toml "github.com/pelletier/go-toml/v2"
)

// quotaTOML mirrors how awsgw.toml nests the limits under a [quota] table.
type quotaTOML struct {
	Quota Limits `toml:"quota"`
}

func TestLimitsTOMLRoundTrip(t *testing.T) {
	const src = `
[quota]
enabled     = true
vcpus       = 8
vpcs        = 8
subnets     = 16
eips        = 2
volumes_gib = 100
`
	want := Limits{
		Enabled:    true,
		VCPUs:      8,
		VPCs:       8,
		Subnets:    16,
		EIPs:       2,
		VolumesGiB: 100,
	}

	var parsed quotaTOML
	if err := toml.Unmarshal([]byte(src), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Quota != want {
		t.Fatalf("parsed = %+v, want %+v", parsed.Quota, want)
	}

	// Marshal then unmarshal again must reproduce the same struct.
	encoded, err := toml.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back quotaTOML
	if err := toml.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if back.Quota != want {
		t.Fatalf("round-trip = %+v, want %+v", back.Quota, want)
	}
}

// A config without a [quota] block must decode to the zero value, which is a
// disabled no-op so existing gateways are unaffected.
func TestLimitsAbsentIsZeroValue(t *testing.T) {
	var parsed quotaTOML
	if err := toml.Unmarshal([]byte("region = \"us-west-1\"\n"), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Quota != (Limits{}) {
		t.Fatalf("absent [quota] = %+v, want zero value", parsed.Quota)
	}
}

func TestExempt(t *testing.T) {
	const normalAccount = "123456789012"
	tests := []struct {
		name      string
		limits    Limits
		accountID string
		want      bool
	}{
		{"system account when enabled", Limits{Enabled: true}, utils.GlobalAccountID, true},
		{"normal account when enabled", Limits{Enabled: true}, normalAccount, false},
		{"normal account when disabled", Limits{Enabled: false}, normalAccount, true},
		{"system account when disabled", Limits{Enabled: false}, utils.GlobalAccountID, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.limits)
			if got := s.Exempt(tt.accountID); got != tt.want {
				t.Errorf("Exempt(%q) = %v, want %v", tt.accountID, got, tt.want)
			}
		})
	}
}
