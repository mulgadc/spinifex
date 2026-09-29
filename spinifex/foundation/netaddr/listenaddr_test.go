package netaddr

import "testing"

func TestDialTarget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 wildcard", "0.0.0.0:4222", "127.0.0.1:4222"},
		{"ipv6 wildcard", "[::]:4222", "[::1]:4222"},
		{"ipv4 loopback passthrough", "127.0.0.1:4222", "127.0.0.1:4222"},
		{"ipv6 loopback passthrough", "[::1]:4222", "[::1]:4222"},
		{"specific ipv4 passthrough", "192.168.1.5:4222", "192.168.1.5:4222"},
		{"hostname passthrough", "nats.cluster.local:4222", "nats.cluster.local:4222"},
		{"empty host rewrites to loopback", ":4222", "127.0.0.1:4222"},
		{"malformed passthrough", "malformed", "malformed"},
		{"ipv4 wildcard tls port", "0.0.0.0:8443", "127.0.0.1:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := DialTarget(tc.in)
			if got != tc.want {
				t.Fatalf("DialTarget(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAdvertiseHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		listen      string
		advertiseIP string
		want        string
	}{
		{"ipv4 wildcard resolves", "0.0.0.0:9999", "192.168.1.5", "192.168.1.5"},
		{"ipv6 wildcard resolves", "[::]:9999", "192.168.1.5", "192.168.1.5"},
		{"specific ip passthrough", "10.11.12.1:9999", "192.168.1.5", "10.11.12.1"},
		{"loopback passthrough", "127.0.0.1:9999", "192.168.1.5", "127.0.0.1"},
		{"hostname passthrough", "nats.cluster.local:9999", "192.168.1.5", "nats.cluster.local"},
		{"no-port input treated as host", "0.0.0.0", "192.168.1.5", "192.168.1.5"},
		{"empty listen falls back to advertise", "", "192.168.1.5", "192.168.1.5"},
		{"empty advertise with wildcard returns empty", "0.0.0.0:9999", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AdvertiseHost(tc.listen, tc.advertiseIP)
			if got != tc.want {
				t.Fatalf("AdvertiseHost(%q, %q) = %q, want %q", tc.listen, tc.advertiseIP, got, tc.want)
			}
		})
	}
}
