package ecr

import "testing"

func TestRegistryAddress(t *testing.T) {
	tests := []struct {
		name      string
		bind      string
		advertise string
		wantHost  string
		wantPort  string
	}{
		{"wildcard bind falls back to advertise", "0.0.0.0:9999", "192.168.0.12", "192.168.0.12", "9999"},
		{"concrete bind equal to advertise", "10.0.0.5:9999", "10.0.0.5", "10.0.0.5", "9999"},
		{"concrete bind wins over a different advertise", "10.0.0.5:9999", "192.168.0.12", "10.0.0.5", "9999"},
		{"ipv6 wildcard falls back to advertise", "[::]:9999", "192.168.0.12", "192.168.0.12", "9999"},
		{"wildcard bind and empty advertise leaves the parity name", "0.0.0.0:9999", "", "", "9999"},
		{"wildcard bind and wildcard advertise leaves the parity name", "0.0.0.0:9999", "0.0.0.0", "", "9999"},
		{"unparseable bind falls back to advertise with no port", "not-a-hostport", "192.168.0.12", "192.168.0.12", ""},
		{"empty bind and empty advertise", "", "", "", ""},
		{"port is taken from the bind", "10.0.0.5:443", "", "10.0.0.5", "443"},
		{"loopback bind counts as concrete", "127.0.0.1:9999", "192.168.0.12", "127.0.0.1", "9999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port := RegistryAddress(tt.bind, tt.advertise)
			if host != tt.wantHost || port != tt.wantPort {
				t.Errorf("RegistryAddress(%q, %q) = (%q, %q), want (%q, %q)",
					tt.bind, tt.advertise, host, port, tt.wantHost, tt.wantPort)
			}
		})
	}
}

func TestRegistryURIHost(t *testing.T) {
	tests := []struct {
		host, port, want string
	}{
		{"10.0.0.1", "9999", "10.0.0.1:9999"},
		{"10.0.0.1", "443", "10.0.0.1"},
		{"10.0.0.1", "", "10.0.0.1"},
	}
	for _, tt := range tests {
		if got := RegistryURIHost(tt.host, tt.port); got != tt.want {
			t.Errorf("RegistryURIHost(%q, %q) = %q, want %q", tt.host, tt.port, got, tt.want)
		}
	}
}
