package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseRegistryHost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		host     string
		suffix   string
		wantAcct string
		wantRegn string
		wantOK   bool
	}{
		{
			name:     "match with suffix",
			host:     "123456789012.dkr.ecr.us-east-1.spinifex.internal",
			suffix:   "spinifex.internal",
			wantAcct: "123456789012",
			wantRegn: "us-east-1",
			wantOK:   true,
		},
		{
			name:     "match strips port",
			host:     "123456789012.dkr.ecr.us-east-1.spinifex.internal:9999",
			suffix:   "spinifex.internal",
			wantAcct: "123456789012",
			wantRegn: "us-east-1",
			wantOK:   true,
		},
		{
			name:     "permissive match when suffix empty",
			host:     "555.dkr.ecr.ap-southeast-2.example.test",
			suffix:   "",
			wantAcct: "555",
			wantRegn: "ap-southeast-2",
			wantOK:   true,
		},
		{
			name:   "control plane host is not a registry host",
			host:   "ecr.us-east-1.spinifex.internal",
			suffix: "spinifex.internal",
			wantOK: false,
		},
		{
			name:   "wrong suffix does not match",
			host:   "123456789012.dkr.ecr.us-east-1.other.internal",
			suffix: "spinifex.internal",
			wantOK: false,
		},
		{
			name:   "missing dkr.ecr labels",
			host:   "123456789012.foo.bar.us-east-1.spinifex.internal",
			suffix: "spinifex.internal",
			wantOK: false,
		},
		{
			name:   "empty host",
			host:   "",
			suffix: "spinifex.internal",
			wantOK: false,
		},
		{
			name:   "too few labels permissive",
			host:   "dkr.ecr.us-east-1",
			suffix: "",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			acct, regn, ok := NewHostMatcher(tc.suffix, "dkr", "ecr").Parse(tc.host)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok {
				if acct != tc.wantAcct {
					t.Errorf("accountID = %q, want %q", acct, tc.wantAcct)
				}
				if regn != tc.wantRegn {
					t.Errorf("region = %q, want %q", regn, tc.wantRegn)
				}
			}
		})
	}
}

func TestHostMatch_PopulatesContextOnRegistryHost(t *testing.T) {
	t.Parallel()

	m := NewHostMatcher("spinifex.internal", "dkr", "ecr")

	var gotAcct, gotRegn string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotAcct, gotRegn = TargetAccount(r.Context()), TargetRegion(r.Context())
	})

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Host = "123456789012.dkr.ecr.us-east-1.spinifex.internal"
	m.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if gotAcct != "123456789012" {
		t.Errorf("target account = %q, want 123456789012", gotAcct)
	}
	if gotRegn != "us-east-1" {
		t.Errorf("target region = %q, want us-east-1", gotRegn)
	}
}

func TestHostMatch_PassesThroughNonRegistryHost(t *testing.T) {
	t.Parallel()

	m := NewHostMatcher("spinifex.internal", "dkr", "ecr")

	var hadAccount bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hadAccount = TargetAccount(r.Context()) != ""
	})

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Host = "ecr.us-east-1.spinifex.internal"
	m.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if hadAccount {
		t.Error("non-registry host should not populate target account context")
	}
}

func TestHostMatcher_LabelsComeFromComposition(t *testing.T) {
	t.Parallel()

	m := NewHostMatcher("example.test", "reg")
	if acct, regn, ok := m.Parse("42.reg.eu-west-1.example.test"); !ok || acct != "42" || regn != "eu-west-1" {
		t.Errorf("Parse = %q %q %v, want 42 eu-west-1 true", acct, regn, ok)
	}
	if _, _, ok := m.Parse("42.dkr.eu-west-1.example.test"); ok {
		t.Error("a host with other labels must not match")
	}
}
