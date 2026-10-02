package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		expected   string
	}{
		{"IPv4 with port", "192.168.1.1:12345", "192.168.1.1"},
		{"IPv6 with port", "[::1]:12345", "::1"},
		{"IPv4 bare", "192.168.1.1", "192.168.1.1"},
		{"IPv6 full with port", "[2001:db8::1]:443", "2001:db8::1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClientIP(tt.remoteAddr); got != tt.expected {
				t.Errorf("ClientIP(%q) = %q, want %q", tt.remoteAddr, got, tt.expected)
			}
		})
	}
}

// X-Real-IP is honoured only on a loopback connection, where the local edge set
// it. From anywhere else it, and X-Forwarded-For, are the client's own claim.
func TestRequestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		realIP     string
		xff        string
		expected   string
	}{
		{"loopback trusts X-Real-IP", "127.0.0.1:40000", "198.51.100.7", "", "198.51.100.7"},
		{"IPv6 loopback trusts X-Real-IP", "[::1]:40000", "2001:db8::7", "", "2001:db8::7"},
		{"4in6 loopback trusts X-Real-IP", "[::ffff:127.0.0.1]:40000", "198.51.100.7", "", "198.51.100.7"},
		{"loopback without header", "127.0.0.1:40000", "", "", "127.0.0.1"},
		{"loopback with garbage header", "127.0.0.1:40000", "not-an-ip", "", "127.0.0.1"},
		{"loopback ignores X-Forwarded-For", "127.0.0.1:40000", "", "203.0.113.1", "127.0.0.1"},
		{"spoofed X-Real-IP from public client", "203.0.113.9:5555", "10.0.0.1", "", "203.0.113.9"},
		{"spoofed X-Forwarded-For from public client", "203.0.113.9:5555", "", "127.0.0.1", "203.0.113.9"},
		{"spoofed both from private client", "10.2.0.3:5555", "127.0.0.1", "127.0.0.1", "10.2.0.3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.realIP != "" {
				r.Header.Set("X-Real-IP", tt.realIP)
			}
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}
			if got := RequestClientIP(r); got != tt.expected {
				t.Errorf("RequestClientIP() = %q, want %q", got, tt.expected)
			}
		})
	}
}
