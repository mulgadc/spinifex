package etcdmember_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit/fakeetcd"
	"github.com/mulgadc/spinifex/spinifex/agents/eks/etcdmember"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var members = []fakeetcd.Member{
	{ID: 0xa1, Name: "cp-a", PeerURLs: []string{"https://10.0.0.10:2380"}},
	{ID: 0xb2, Name: "cp-b", PeerURLs: []string{"https://10.0.0.12:2380"}},
}

func dial(t *testing.T, srv *fakeetcd.Server) *etcdmember.EtcdClient {
	t.Helper()
	cli, err := etcdmember.Dial(srv.Endpoint, srv.CertDir, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestEtcdClientListsMembersOverMutualTLS(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, members)
	cli := dial(t, srv)

	got, err := cli.MemberList(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, etcdmember.Member{ID: 0xb2, Name: "cp-b", PeerURLs: []string{"https://10.0.0.12:2380"}}, got[1])
}

func TestEtcdClientSelfIDIsTheAnsweringMember(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, members)

	id, err := dial(t, srv).SelfID(t.Context())
	require.NoError(t, err)
	assert.Equal(t, uint64(0xa1), id)
}

func TestEtcdClientRemovesTheRequestedMember(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, members)

	require.NoError(t, dial(t, srv).MemberRemove(t.Context(), 0xb2))
	assert.Equal(t, []uint64{0xb2}, srv.Removed())
}

func TestEtcdClientSurfacesServerErrors(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, members)
	cli := dial(t, srv)
	srv.FailWith(errors.New("etcdserver: unhealthy cluster"))

	_, err := cli.MemberList(t.Context())
	assert.ErrorContains(t, err, "unhealthy cluster")
	assert.ErrorContains(t, cli.MemberRemove(t.Context(), 0xb2), "unhealthy cluster")
	_, err = cli.SelfID(t.Context())
	assert.ErrorContains(t, err, "unhealthy cluster")
	assert.Empty(t, srv.Removed())
}

func TestDialFailsWithoutClientCertificates(t *testing.T) {
	t.Parallel()
	_, err := etcdmember.Dial("https://127.0.0.1:2379", t.TempDir(), time.Second)
	require.Error(t, err)
	assert.ErrorContains(t, err, "etcd client TLS")
}

func TestRunRemovesTheDeadPeerThroughARealClient(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, members)
	var out bytes.Buffer

	code := etcdmember.Run(context.Background(), dial(t, srv),
		etcdmember.Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"}, &out)

	assert.Zero(t, code, out.String())
	assert.Equal(t, []uint64{0xb2}, srv.Removed())
}

func TestRunRefusesToRemoveTheLocalMemberThroughARealClient(t *testing.T) {
	t.Parallel()
	srv := fakeetcd.Start(t, 0xa1, members)
	var out bytes.Buffer

	code := etcdmember.Run(context.Background(), dial(t, srv), etcdmember.Config{PeerIP: "10.0.0.10"}, &out)

	assert.NotZero(t, code)
	assert.Empty(t, srv.Removed())
}
