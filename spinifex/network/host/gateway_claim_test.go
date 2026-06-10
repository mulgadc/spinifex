package host

import (
	"slices"
	"testing"
)

func TestGatewayClaimArgs(t *testing.T) {
	base := []string{"--columns=logical_port,type,chassis,up,requested_chassis",
		"find", "Port_Binding", "logical_port=gw-vpc-a"}

	got := gatewayClaimArgs("", "gw-vpc-a")
	want := append([]string{"--no-leader-only"}, base...)
	if !slices.Equal(got, want) {
		t.Errorf("no sbAddr: got %v, want %v", got, want)
	}

	got = gatewayClaimArgs("tcp:1.2.3.4:6642", "gw-vpc-a")
	want = append([]string{"--no-leader-only", "--db=tcp:1.2.3.4:6642"}, base...)
	if !slices.Equal(got, want) {
		t.Errorf("with sbAddr: got %v, want %v", got, want)
	}
}

func TestChassisClaimed(t *testing.T) {
	tests := []struct {
		name string
		row  string
		want bool
	}{
		{
			name: "claimed single chassis",
			row:  "logical_port        : gw-vpc-a\ntype                : l3gateway\nchassis             : 891a28a4-1111\nup                  : true\nrequested_chassis   : 891a28a4-1111",
			want: true,
		},
		{
			name: "unclaimed empty set",
			row:  "logical_port        : gw-vpc-a\ntype                : l3gateway\nchassis             : []\nup                  : false\nrequested_chassis   : []",
			want: false,
		},
		{
			name: "unclaimed empty string",
			row:  "chassis             : ",
			want: false,
		},
		{
			name: "no chassis column",
			row:  "logical_port        : gw-vpc-a\ntype                : l3gateway",
			want: false,
		},
		{
			name: "empty row",
			row:  "",
			want: false,
		},
		{
			name: "claimed amid other columns",
			row:  "up                  : true\nchassis             : abc-def\ntype                : l3gateway",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := chassisClaimed(tt.row); got != tt.want {
				t.Errorf("chassisClaimed(%q) = %v, want %v", tt.row, got, tt.want)
			}
		})
	}
}
