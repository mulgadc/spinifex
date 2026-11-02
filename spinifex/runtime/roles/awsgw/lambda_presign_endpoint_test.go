package awsgw

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLambdaPresignEndpoint(t *testing.T) {
	cases := []struct{ name, host, advertise, want string }{
		{"wildcard uses the advertise IP", "0.0.0.0:8443", "192.168.0.15", "https://192.168.0.15:8443"},
		{"loaded wildcard (loopback) uses the advertise IP", "127.0.0.1:8443", "192.168.0.15", "https://192.168.0.15:8443"},
		{"localhost uses the advertise IP", "localhost:8443", "192.168.0.15", "https://192.168.0.15:8443"},
		{"concrete listen host is kept", "10.22.7.1:8443", "192.168.0.15", "https://10.22.7.1:8443"},
		{"wildcard with no advertise IP is unknown", "0.0.0.0:8443", "", ""},
		{"loopback with no advertise IP is unknown", "127.0.0.1:8443", "", ""},
		{"no port is unknown", "192.168.0.15", "192.168.0.15", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, lambdaPresignEndpoint(tc.host, tc.advertise))
		})
	}
}
