package utils

import "testing"

func TestPlatformFromDetails(t *testing.T) {
	tests := []struct {
		name            string
		platformDetails string
		want            string
	}{
		{name: "windows", platformDetails: "Windows", want: PlatformWindows},
		{name: "windows byol", platformDetails: "Windows BYOL", want: PlatformWindows},
		{name: "windows with sql", platformDetails: "Windows with SQL Server Standard", want: PlatformWindows},
		{name: "lowercase windows", platformDetails: "windows", want: PlatformWindows},
		{name: "linux", platformDetails: "Linux/UNIX", want: ""},
		{name: "rhel", platformDetails: "Red Hat Enterprise Linux", want: ""},
		{name: "empty", platformDetails: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PlatformFromDetails(tt.platformDetails)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("PlatformFromDetails(%q) = %q, want nil", tt.platformDetails, *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("PlatformFromDetails(%q) = nil, want %q", tt.platformDetails, tt.want)
			}
			if *got != tt.want {
				t.Fatalf("PlatformFromDetails(%q) = %q, want %q", tt.platformDetails, *got, tt.want)
			}
		})
	}
}
