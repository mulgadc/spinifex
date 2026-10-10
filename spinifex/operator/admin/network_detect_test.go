package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSuggestPoolRange(t *testing.T) {
	tests := []struct {
		name      string
		subnet    string
		wantStart string
		wantEnd   string
	}{
		{
			// /23 broadcast 192.168.1.255 → end .250, start .150 (borrows across octet)
			name:      "Slash23BorrowsAcrossOctet",
			subnet:    "192.168.0.0/23",
			wantStart: "192.168.1.150",
			wantEnd:   "192.168.1.250",
		},
		{
			// /24 broadcast 192.168.1.255 → end .250, start .150
			name:      "Slash24",
			subnet:    "192.168.1.0/24",
			wantStart: "192.168.1.150",
			wantEnd:   "192.168.1.250",
		},
		{
			name:      "InvalidCIDRReturnsEmpty",
			subnet:    "not-a-cidr",
			wantStart: "",
			wantEnd:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := SuggestPoolRange(&DetectedInterface{Subnet: tt.subnet})
			assert.Equal(t, tt.wantStart, start)
			assert.Equal(t, tt.wantEnd, end)
		})
	}
}

func TestIsVirtualInterface(t *testing.T) {
	tests := []struct {
		name            string
		iface           string
		defaultRouteDev string
		want            bool
	}{
		{
			// Default-route override: a bridge name normally treated as virtual
			// is considered physical when it's the WAN uplink.
			name:            "DefaultRouteOverridesVirtualPrefix",
			iface:           "br-wan",
			defaultRouteDev: "br-wan",
			want:            false,
		},
		{
			name:            "DockerPrefixIsVirtual",
			iface:           "docker0",
			defaultRouteDev: "enp0s3",
			want:            true,
		},
		{
			name:            "PhysicalNICNotVirtual",
			iface:           "enp0s3",
			defaultRouteDev: "enp0s3",
			want:            false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, isVirtualInterface(tt.iface, tt.defaultRouteDev))
		})
	}
}
