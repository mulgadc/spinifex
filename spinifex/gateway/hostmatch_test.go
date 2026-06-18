package gateway

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
			host:     "123456789012.dkr.ecr.us-east-1.mulga.internal",
			suffix:   "mulga.internal",
			wantAcct: "123456789012",
			wantRegn: "us-east-1",
			wantOK:   true,
		},
		{
			name:     "match strips port",
			host:     "123456789012.dkr.ecr.us-east-1.mulga.internal:9999",
			suffix:   "mulga.internal",
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
			host:   "ecr.us-east-1.mulga.internal",
			suffix: "mulga.internal",
			wantOK: false,
		},
		{
			name:   "wrong suffix does not match",
			host:   "123456789012.dkr.ecr.us-east-1.other.internal",
			suffix: "mulga.internal",
			wantOK: false,
		},
		{
			name:   "missing dkr.ecr labels",
			host:   "123456789012.foo.bar.us-east-1.mulga.internal",
			suffix: "mulga.internal",
			wantOK: false,
		},
		{
			name:   "empty host",
			host:   "",
			suffix: "mulga.internal",
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
			acct, regn, ok := parseRegistryHost(tc.host, tc.suffix)
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

	gw := &GatewayConfig{InternalSuffix: "mulga.internal"}

	var gotAcct, gotRegn string
	var acctOK, regnOK bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotAcct, acctOK = r.Context().Value(ctxTargetAccount).(string)
		gotRegn, regnOK = r.Context().Value(ctxTargetRegion).(string)
	})

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Host = "123456789012.dkr.ecr.us-east-1.mulga.internal"
	gw.hostMatch(next).ServeHTTP(httptest.NewRecorder(), req)

	if !acctOK || gotAcct != "123456789012" {
		t.Errorf("target account = %q (ok=%v), want 123456789012", gotAcct, acctOK)
	}
	if !regnOK || gotRegn != "us-east-1" {
		t.Errorf("target region = %q (ok=%v), want us-east-1", gotRegn, regnOK)
	}
}

func TestHostMatch_PassesThroughNonRegistryHost(t *testing.T) {
	t.Parallel()

	gw := &GatewayConfig{InternalSuffix: "mulga.internal"}

	var hadAccount bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, hadAccount = r.Context().Value(ctxTargetAccount).(string)
	})

	req := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	req.Host = "ecr.us-east-1.mulga.internal"
	gw.hostMatch(next).ServeHTTP(httptest.NewRecorder(), req)

	if hadAccount {
		t.Error("non-registry host should not populate target account context")
	}
}
