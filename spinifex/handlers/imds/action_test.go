package handlers_imds

import (
	"net/http"
	"testing"
)

func TestIMDSActionNamesKnownEndpoints(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{name: "token issue", method: http.MethodPut, path: "/latest/api/token", want: "imds.api.token"},
		{name: "token wrong method", method: http.MethodGet, path: "/latest/api/token", want: "imds.api.token"},
		{name: "ca cert", method: http.MethodGet, path: "/spinifex/ca.pem", want: "imds.spinifex.ca-cert"},
		{name: "user data", method: http.MethodGet, path: "/latest/user-data", want: "imds.user-data"},
		{name: "version listing", method: http.MethodGet, path: "/", want: "imds.versions"},
		{name: "latest listing", method: http.MethodGet, path: "/latest", want: "imds.latest"},
		{name: "latest listing trailing slash", method: http.MethodGet, path: "/latest/", want: "imds.latest"},
		{name: "identity document", method: http.MethodGet, path: "/latest/dynamic/instance-identity/document", want: "imds.dynamic.instance-identity.document"},
		{name: "identity dir", method: http.MethodGet, path: "/latest/dynamic/instance-identity", want: "imds.dynamic.instance-identity"},
		{name: "meta-data root", method: http.MethodGet, path: "/latest/meta-data", want: "imds.meta-data.root"},
		{name: "meta-data root trailing slash", method: http.MethodGet, path: "/latest/meta-data/", want: "imds.meta-data.root"},
		{name: "instance id", method: http.MethodGet, path: "/latest/meta-data/instance-id", want: "imds.meta-data.instance-id"},
		{name: "availability zone", method: http.MethodGet, path: "/latest/meta-data/placement/availability-zone", want: "imds.meta-data.placement.availability-zone"},
		{name: "iam info", method: http.MethodGet, path: "/latest/meta-data/iam/info", want: "imds.meta-data.iam.info"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := imdsAction(tc.method, tc.path); got != tc.want {
				t.Errorf("imdsAction(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

// TestIMDSActionStripsResourceIdentifiers is the cardinality guard: a guest
// controls these path segments, so any of them reaching the metric dimension
// would produce one series per role, per MAC or per invented path.
func TestIMDSActionStripsResourceIdentifiers(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "role name stripped", path: "/latest/meta-data/iam/security-credentials/my-app-role", want: "imds.iam.security-credentials.role"},
		{name: "other role same action", path: "/latest/meta-data/iam/security-credentials/another-role", want: "imds.iam.security-credentials.role"},
		{name: "credentials listing", path: "/latest/meta-data/iam/security-credentials", want: "imds.iam.security-credentials.list"},
		{name: "credentials listing trailing slash", path: "/latest/meta-data/iam/security-credentials/", want: "imds.iam.security-credentials.list"},
		{name: "mac stripped", path: "/latest/meta-data/network/interfaces/macs/0a:1b:2c:3d:4e:5f/vpc-id", want: "imds.meta-data.network.interfaces.macs"},
		{name: "other mac same action", path: "/latest/meta-data/network/interfaces/macs/aa:bb:cc:dd:ee:ff/subnet-id", want: "imds.meta-data.network.interfaces.macs"},
		{name: "macs listing", path: "/latest/meta-data/network/interfaces/macs", want: "imds.meta-data.network.interfaces.macs"},
		{name: "public key index stripped", path: "/latest/meta-data/public-keys/0/openssh-key", want: "imds.public-keys"},
		{name: "public keys listing", path: "/latest/meta-data/public-keys", want: "imds.public-keys"},
		{name: "unknown meta-data key", path: "/latest/meta-data/not-a-real-key", want: actionOther},
		{name: "invented deep path", path: "/latest/meta-data/a/b/c/d/e", want: actionOther},
		{name: "path outside the tree", path: "/etc/passwd", want: actionOther},
		{name: "empty path", path: "", want: actionOther},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := imdsAction(http.MethodGet, tc.path); got != tc.want {
				t.Errorf("imdsAction(GET, %q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}
