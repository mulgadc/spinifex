package gatewayfetch

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/aws/aws-sdk-go/private/protocol/json/jsonutil"
	eksv1 "github.com/mulgadc/spinifex/contracts/eks/v1"
)

// The gateway sends Go field names with empty strings present, not the
// camelCase json tags; decoding relies on case-insensitive field matching.
func TestEmitAddonsTSV_GatewayWireBody(t *testing.T) {
	body := []byte(`{"Addons":[` +
		`{"AddonName":"aws-load-balancer-controller","AddonVersion":"2.11.0",` +
		`"ServiceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","ConfigurationValues":"{\"a\":\"x\ty\nz\"}"},` +
		`{"AddonName":"spinifex-noop","AddonVersion":"0.1.0","ServiceAccountRoleArn":"","ConfigurationValues":""}]}`)

	var buf bytes.Buffer
	if err := emitAddonsTSV(&buf, body); err != nil {
		t.Fatalf("emitAddonsTSV: %v", err)
	}
	cfg := base64.StdEncoding.EncodeToString([]byte("{\"a\":\"x\ty\nz\"}"))
	want := "aws-load-balancer-controller\t2.11.0\tarn:aws:iam::123456789012:role/alb\t" + cfg + "\n" +
		"spinifex-noop\t0.1.0\t\t\n"
	if buf.String() != want {
		t.Fatalf("TSV mismatch:\n got: %q\nwant: %q", buf.String(), want)
	}
}

// The contract type rendered by the gateway's marshaller must yield the same
// TSV, including the empty set the gateway sends for a cluster with no add-ons.
func TestEmitAddonsTSV_ContractRenderedBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp eksv1.InternalAddonsResponse
		want string
	}{
		{
			name: "staged",
			resp: eksv1.InternalAddonsResponse{Addons: []eksv1.StagedAddonManifest{
				{AddonName: "aws-load-balancer-controller", AddonVersion: "2.11.0",
					ServiceAccountRoleArn: "arn:aws:iam::123456789012:role/alb", ConfigurationValues: "a\tb"},
				{AddonName: "spinifex-noop", AddonVersion: "0.1.0"},
			}},
			want: "aws-load-balancer-controller\t2.11.0\tarn:aws:iam::123456789012:role/alb\tYQli\n" +
				"spinifex-noop\t0.1.0\t\t\n",
		},
		{name: "empty", resp: eksv1.InternalAddonsResponse{Addons: []eksv1.StagedAddonManifest{}}, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := jsonutil.BuildJSON(&tc.resp)
			if err != nil {
				t.Fatalf("BuildJSON: %v", err)
			}
			var buf bytes.Buffer
			if err := emitAddonsTSV(&buf, body); err != nil {
				t.Fatalf("emitAddonsTSV: %v", err)
			}
			if buf.String() != tc.want {
				t.Fatalf("TSV mismatch:\n got: %q\nwant: %q", buf.String(), tc.want)
			}
		})
	}
}
