package awsgw_test

import (
	"github.com/mulgadc/spinifex/internal/testkit/gwfix"
	"github.com/mulgadc/spinifex/spinifex/gateway"
	"github.com/mulgadc/spinifex/spinifex/ingress/aws/reqctx"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetService(t *testing.T) {
	gw := withEC2Verifier(gwfix.WithDeps(&gateway.GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	withServices(gw, taggingRegistration(nil), iamRegistration(gw), ec2Registration(gw))

	tests := []struct {
		name      string
		ctxVal    string // service to record, empty means none
		path      string // request path, empty means "/"
		wantSvc   string
		wantError string
	}{
		{
			name:      "no service in context",
			wantError: awserrors.ErrorAuthFailure,
		},
		{
			name:      "unsupported service",
			ctxVal:    "s3",
			wantError: awserrors.ErrorUnsupportedOperation,
		},
		{
			name:    "ec2 service",
			ctxVal:  "ec2",
			wantSvc: "ec2",
		},
		{
			name:    "iam service",
			ctxVal:  "iam",
			wantSvc: "iam",
		},
		{
			name:    "tagging service",
			ctxVal:  "tagging",
			wantSvc: "tagging",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/"
			}
			req := httptest.NewRequest(http.MethodPost, path, nil)
			if tc.ctxVal != "" {
				ctx := reqctx.WithService(req.Context(), tc.ctxVal)
				req = req.WithContext(ctx)
			}

			svc, err := gw.GetService(req)
			if tc.wantError != "" {
				require.Error(t, err)
				assert.Equal(t, tc.wantError, err.Error())
				assert.Empty(t, svc)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantSvc, svc)
			}
		})
	}
}

func TestRequest_NoServiceContext(t *testing.T) {
	gw := withEC2Verifier(gwfix.WithDeps(&gateway.GatewayConfig{DisableLogging: true}, gwfix.Deps{IAM: allowAllIAMService()}))
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	w := httptest.NewRecorder()

	gw.Request(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)
	assert.Equal(t, 403, resp.StatusCode)
	assert.Contains(t, string(body), "AuthFailure")
}
