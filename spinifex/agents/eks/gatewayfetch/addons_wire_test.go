package gatewayfetch

import (
	"bytes"
	"encoding/base64"
	"testing"
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
