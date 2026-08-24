package arn

import "testing"

func TestFormatIAMPath(t *testing.T) {
	tests := []struct {
		name         string
		kind         IAMResourceType
		resourcePath string
		resourceName string
		want         string
	}{
		{"empty path defaults to root", IAMUser, "", "alice", "arn:aws:iam::123456789012:user/alice"},
		{"root path", IAMPolicy, "/", "app", "arn:aws:iam::123456789012:policy/app"},
		{"nested path", IAMRole, "/service-roles/", "app", "arn:aws:iam::123456789012:role/service-roles/app"},
		{"path is not normalized", IAMGroup, "/teams//ops/", "admins", "arn:aws:iam::123456789012:group/teams//ops/admins"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatIAMPath(tt.kind, "123456789012", tt.resourcePath, tt.resourceName)
			if got != tt.want {
				t.Fatalf("FormatIAMPath() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatIAMResource(t *testing.T) {
	got := FormatIAMResource(IAMOIDCProvider, "123456789012", "issuer.example/id/cluster")
	want := "arn:aws:iam::123456789012:oidc-provider/issuer.example/id/cluster"
	if got != want {
		t.Fatalf("FormatIAMResource() = %q, want %q", got, want)
	}
}
