package main

import (
	"bytes"
	"testing"

	"github.com/mulgadc/spinifex/internal/testkit/fakeetcd"
	"github.com/stretchr/testify/assert"
)

var fakeMembers = []fakeetcd.Member{
	{ID: 0xa1, Name: "cp-a", PeerURLs: []string{"https://10.0.0.10:2380"}},
	{ID: 0xb2, Name: "cp-b", PeerURLs: []string{"https://10.0.0.12:2380"}},
}

func TestRunRequiresTheRemoveSubcommand(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"list"}} {
		var out, errOut bytes.Buffer
		assert.Equal(t, 2, run(args, &out, &errOut))
		assert.Contains(t, errOut.String(), "usage: eks-etcd-member remove")
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	assert.Equal(t, 2, run([]string{"remove", "--bogus"}, &out, &errOut))
}

func TestRunFailsWhenEtcdCertificatesAreMissing(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	code := run([]string{"remove", "--peer-ip", "10.0.0.12", "--cert-dir", t.TempDir()}, &out, &errOut)
	assert.Equal(t, 1, code)
	assert.Contains(t, errOut.String(), "etcd member remove failed")
}

func TestRunRemovesTheNamedPeer(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, fakeMembers)
	var out, errOut bytes.Buffer

	code := run([]string{"remove", "--peer-ip", "10.0.0.12", "--self-ip", "10.0.0.10",
		"--endpoint", srv.Endpoint, "--cert-dir", srv.CertDir}, &out, &errOut)

	assert.Zero(t, code, errOut.String()+out.String())
	assert.Contains(t, out.String(), "removed etcd member 10.0.0.12")
	assert.Equal(t, []uint64{0xb2}, srv.Removed())
}

func TestRunTreatsAnAbsentPeerAsSuccess(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, fakeMembers)
	var out, errOut bytes.Buffer

	code := run([]string{"remove", "--peer-ip", "10.0.0.99",
		"--endpoint", srv.Endpoint, "--cert-dir", srv.CertDir}, &out, &errOut)

	assert.Zero(t, code)
	assert.Contains(t, out.String(), "member already absent")
	assert.Empty(t, srv.Removed())
}
