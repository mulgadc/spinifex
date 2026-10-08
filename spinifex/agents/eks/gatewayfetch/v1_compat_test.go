package gatewayfetch

import (
	"bytes"
	"testing"
)

// Fields a later gateway adds never reach the four TSV columns the on-VM
// addon-sync agent reads.
func TestEmitAddonsTSV_IgnoresUnknownFields(t *testing.T) {
	body := []byte(`{"Addons":[{"AddonName":"aws-load-balancer-controller","AddonVersion":"2.11.0",` +
		`"ServiceAccountRoleArn":"arn:aws:iam::123456789012:role/alb","ConfigurationValues":"a\tb",` +
		`"IncarnationId":"inc-1","Generation":4}],"ExecutorEpoch":7}`)

	var buf bytes.Buffer
	if err := emitAddonsTSV(&buf, body); err != nil {
		t.Fatalf("emitAddonsTSV: %v", err)
	}
	want := "aws-load-balancer-controller\t2.11.0\tarn:aws:iam::123456789012:role/alb\tYQli\n"
	if buf.String() != want {
		t.Fatalf("TSV mismatch:\n got: %q\nwant: %q", buf.String(), want)
	}
}
