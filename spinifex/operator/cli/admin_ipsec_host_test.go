//test:in-package — resolveIPSecForHost is an unexported guard on this command's
// flags, and exporting it would widen the package's API for a test's convenience.

package cli

import (
	"strings"
	"testing"
)

// A libreswan host cannot run OVN IPsec, so it defaults to off and refuses an
// explicit request; every other host keeps whatever --ipsec says.
func TestResolveIPSecForHost(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		requested, explicit, libreswan bool
		want                           bool
		wantErr                        bool
	}{
		{name: "strongSwan default", requested: true, want: true},
		{name: "strongSwan explicit off", explicit: true},
		{name: "libreswan default", requested: true, libreswan: true},
		{name: "libreswan explicit off", explicit: true, libreswan: true},
		{name: "libreswan explicit on", requested: true, explicit: true, libreswan: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveIPSecForHost(tc.requested, tc.explicit, tc.libreswan)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want an error, got nil")
				}
				// The operator needs to know why, and what to run instead.
				for _, word := range []string{"libreswan", "/usr/libexec/ipsec/pluto", "without --ipsec"} {
					if !strings.Contains(err.Error(), word) {
						t.Errorf("error does not name %q: %v", word, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
