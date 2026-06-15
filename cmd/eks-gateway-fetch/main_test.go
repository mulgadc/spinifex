package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestEmitAddonsTSV(t *testing.T) {
	body := []byte(`{"addons":[
		{"addonName":"spinifex-noop","addonVersion":"0.1.0"},
		{"addonName":"aws-load-balancer-controller","addonVersion":"2.8.1",
		 "serviceAccountRoleArn":"arn:aws:iam::111122223333:role/alb",
		 "configurationValues":"{\"replicaCount\":2}"}
	]}`)

	var buf bytes.Buffer
	if err := emitAddonsTSV(&buf, body); err != nil {
		t.Fatalf("emitAddonsTSV: %v", err)
	}
	cfg := base64.StdEncoding.EncodeToString([]byte(`{"replicaCount":2}`))
	want := "spinifex-noop\t0.1.0\t\t\n" +
		"aws-load-balancer-controller\t2.8.1\tarn:aws:iam::111122223333:role/alb\t" + cfg + "\n"
	if buf.String() != want {
		t.Fatalf("TSV mismatch:\n got: %q\nwant: %q", buf.String(), want)
	}
}

func TestEmitAddonsTSV_Empty(t *testing.T) {
	var buf bytes.Buffer
	if err := emitAddonsTSV(&buf, []byte(`{"addons":[]}`)); err != nil {
		t.Fatalf("emitAddonsTSV: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty addon set must emit nothing, got %q", buf.String())
	}
}

func TestEmitAddonsTSV_BadJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := emitAddonsTSV(&buf, []byte(`{not-json`)); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}
