package etcdmember

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClient struct {
	members   []Member
	listErr   error
	removeErr error
	removed   []uint64
	selfID    uint64
	selfErr   error
}

func (f *fakeClient) MemberList(context.Context) ([]Member, error) {
	return f.members, f.listErr
}

func (f *fakeClient) MemberRemove(_ context.Context, id uint64) error {
	f.removed = append(f.removed, id)
	return f.removeErr
}

func (f *fakeClient) SelfID(context.Context) (uint64, error) {
	return f.selfID, f.selfErr
}

var _ Client = (*fakeClient)(nil)

func threeMembers() []Member {
	return []Member{
		{ID: 1, Name: "cp-1", PeerURLs: []string{"https://10.0.0.10:2380"}},
		{ID: 2, Name: "cp-2", PeerURLs: []string{"https://10.0.0.12:2380"}},
		{ID: 3, Name: "cp-3", PeerURLs: []string{"https://10.0.0.11:2380"}},
	}
}

func TestRemove_RemovesOnlyTheMemberWithThatPeerIP(t *testing.T) {
	c := &fakeClient{members: threeMembers()}
	require.NoError(t, Remove(context.Background(), c, Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"}))
	assert.Equal(t, []uint64{2}, c.removed)
}

func TestRemove_MatchesTheHostNotAPrefix(t *testing.T) {
	c := &fakeClient{members: []Member{
		{ID: 1, Name: "cp-1", PeerURLs: []string{"https://10.0.0.10:2380"}},
		{ID: 2, Name: "cp-2", PeerURLs: []string{"https://10.0.0.123:2380"}},
	}}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"})
	require.ErrorIs(t, err, ErrNoMatch)
	assert.Empty(t, c.removed)
}

func TestRemove_AnyAdvertisedPeerURLCounts(t *testing.T) {
	c := &fakeClient{members: []Member{
		{ID: 1, Name: "cp-1", PeerURLs: []string{"https://10.0.0.10:2380"}},
		{ID: 2, Name: "cp-2", PeerURLs: []string{"https://127.0.0.1:2380", "https://10.0.0.12:2380"}},
	}}
	require.NoError(t, Remove(context.Background(), c, Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"}))
	assert.Equal(t, []uint64{2}, c.removed)
}

func TestRemove_RefusesTheLocalMemberByIDWithoutASelfIP(t *testing.T) {
	c := &fakeClient{members: threeMembers(), selfID: 2}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.12"})
	require.ErrorIs(t, err, ErrSelf)
	assert.Empty(t, c.removed)
}

func TestRemove_RemovesNothingWhenTheLocalMemberCannotBeIdentified(t *testing.T) {
	boom := errors.New("status unavailable")
	c := &fakeClient{members: threeMembers(), selfErr: boom}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.12"})
	require.ErrorIs(t, err, boom)
	assert.Empty(t, c.removed)
}

func TestRemove_RefusesWhenNothingMatches(t *testing.T) {
	c := &fakeClient{members: threeMembers()}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.99", SelfIP: "10.0.0.10"})
	require.ErrorIs(t, err, ErrNoMatch)
	assert.Empty(t, c.removed)
}

func TestRemove_RefusesWhenMoreThanOneMatches(t *testing.T) {
	c := &fakeClient{members: []Member{
		{ID: 1, Name: "cp-1", PeerURLs: []string{"https://10.0.0.10:2380"}},
		{ID: 2, Name: "cp-2", PeerURLs: []string{"https://10.0.0.12:2380"}},
		{ID: 3, Name: "cp-2-old", PeerURLs: []string{"https://10.0.0.12:2380"}},
	}}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"})
	require.ErrorIs(t, err, ErrAmbiguous)
	assert.Empty(t, c.removed)
}

func TestRemove_RefusesToRemoveItself(t *testing.T) {
	c := &fakeClient{members: threeMembers()}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.10", SelfIP: "10.0.0.10"})
	require.ErrorIs(t, err, ErrSelf)
	assert.Empty(t, c.removed)
}

func TestRemove_ListFailureRemovesNothing(t *testing.T) {
	boom := errors.New("etcd unreachable")
	c := &fakeClient{members: threeMembers(), listErr: boom}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"})
	require.ErrorIs(t, err, boom)
	assert.Empty(t, c.removed)
}

func TestRemove_RemoveFailureIsReturned(t *testing.T) {
	boom := errors.New("member not found")
	c := &fakeClient{members: threeMembers(), removeErr: boom}
	err := Remove(context.Background(), c, Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, []uint64{2}, c.removed)
}

func TestRun_ExitCodeMapping(t *testing.T) {
	boom := errors.New("etcd unreachable")
	cfg := Config{PeerIP: "10.0.0.12", SelfIP: "10.0.0.10"}
	cases := []struct {
		name     string
		client   *fakeClient
		cfg      Config
		wantCode int
		wantOut  string
	}{
		{"removed", &fakeClient{members: threeMembers()}, cfg, 0, "removed etcd member 10.0.0.12"},
		{"already absent is success", &fakeClient{members: threeMembers()}, Config{PeerIP: "10.0.0.99", SelfIP: "10.0.0.10"}, 0, "member already absent: 10.0.0.99"},
		{"self refused", &fakeClient{members: threeMembers()}, Config{PeerIP: "10.0.0.10", SelfIP: "10.0.0.10"}, 1, "refusing to remove the local etcd member"},
		{"ambiguous refused", &fakeClient{members: append(threeMembers(), Member{ID: 4, Name: "dup", PeerURLs: []string{"https://10.0.0.12:2380"}})}, cfg, 1, "more than one etcd member"},
		{"list error", &fakeClient{listErr: boom}, cfg, 1, "etcd unreachable"},
		{"remove error", &fakeClient{members: threeMembers(), removeErr: boom}, cfg, 1, "etcd unreachable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			assert.Equal(t, tc.wantCode, Run(context.Background(), tc.client, tc.cfg, &out))
			assert.Contains(t, out.String(), tc.wantOut)
		})
	}
}
