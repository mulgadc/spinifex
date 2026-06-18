package ecr

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/testutil"
	"github.com/mulgadc/spinifex/spinifex/utils"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const svcTestAccount = "000000000000"

// TestMetaServiceImpl_FlagMapping checks the daemon-side service maps store
// ErrNotFound/ErrConflict into the response flags rather than transport errors,
// so they survive the AWS-error-code-only NATS reply.
func TestMetaServiceImpl_FlagMapping(t *testing.T) {
	svc := NewMetaServiceImpl(NewMemoryMetaStore())

	// Absent repo -> Found:false, nil error.
	dr, err := svc.RepoDescribe(&RepoDescribeRequest{Repo: "ghost"}, svcTestAccount)
	require.NoError(t, err)
	assert.False(t, dr.Found)

	_, err = svc.RepoCreate(&RepoCreateRequest{Meta: RepoMeta{Name: "team/app", CreatedAt: time.Now()}}, svcTestAccount)
	require.NoError(t, err)
	dr, err = svc.RepoDescribe(&RepoDescribeRequest{Repo: "team/app"}, svcTestAccount)
	require.NoError(t, err)
	assert.True(t, dr.Found)

	// Absent tag/manifest/upload all report Found:false.
	tg, err := svc.TagGet(&TagGetRequest{Repo: "team/app", Tag: "v1"}, svcTestAccount)
	require.NoError(t, err)
	assert.False(t, tg.Found)
	td, err := svc.TagDelete(&TagDeleteRequest{Repo: "team/app", Tag: "v1"}, svcTestAccount)
	require.NoError(t, err)
	assert.False(t, td.Found)
	ug, err := svc.UploadGet(&UploadGetRequest{UploadID: "nope"}, svcTestAccount)
	require.NoError(t, err)
	assert.False(t, ug.Found)

	// Upload CAS conflict -> Conflict:true, Found:true.
	cr, err := svc.UploadCreate(&UploadCreateRequest{UploadID: "u1", State: UploadState{RepoName: "team/app"}}, svcTestAccount)
	require.NoError(t, err)
	uu, err := svc.UploadUpdate(&UploadUpdateRequest{UploadID: "u1", State: UploadState{CommittedBytes: 5}, Revision: cr.Revision + 99}, svcTestAccount)
	require.NoError(t, err)
	assert.True(t, uu.Found)
	assert.True(t, uu.Conflict)
}

// TestNATSMetaStore_RepoPolicy drives the policy passthrough round-trip
// (put/get/delete + not-found mapping) end to end over the embedded server.
func TestNATSMetaStore_RepoPolicy(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	svc := NewKVMetaService(js)

	serveMeta(t, nc, SubjectPolicyPut, svc.PolicyPut)
	serveMeta(t, nc, SubjectPolicyGet, svc.PolicyGet)
	serveMeta(t, nc, SubjectPolicyDelete, svc.PolicyDelete)

	var client MetaStore = NewNATSMetaStore(nc)
	const acct = svcTestAccount
	const policy = `{"Version":"2012-10-17"}`

	// Unset policy round-trips as ErrNotFound for both get and delete.
	_, err := client.GetRepoPolicy(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
	_, err = client.DeleteRepoPolicy(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, client.PutRepoPolicy(acct, "team/app", []byte(policy)))
	got, err := client.GetRepoPolicy(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(got))

	deleted, err := client.DeleteRepoPolicy(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, policy, string(deleted))
	_, err = client.GetRepoPolicy(acct, "team/app")
	assert.ErrorIs(t, err, ErrNotFound)
}

// serveMeta wires a MetaService method to a NATS subject, mirroring the daemon's
// handleNATSRequest (unmarshal -> service(accountID) -> JSON or error payload).
func serveMeta[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(*I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		accountID := utils.AccountIDFromMsg(msg)
		in := new(I)
		if errResp := utils.UnmarshalJsonPayload(in, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		out, err := fn(in, accountID)
		if err != nil {
			_ = msg.Respond(utils.GenerateErrorPayload("ServerInternal"))
			return
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

// TestNATSMetaStore_RoundTrip drives the gateway client (NATSMetaStore) against
// a KV-backed daemon service over an embedded NATS/JetStream server, proving the
// full request/reply path including ErrNotFound/ErrConflict reconstruction.
func TestNATSMetaStore_RoundTrip(t *testing.T) {
	_, nc, js := testutil.StartTestJetStream(t)
	svc := NewKVMetaService(js)

	serveMeta(t, nc, SubjectRepoCreate, svc.RepoCreate)
	serveMeta(t, nc, SubjectRepoDescribe, svc.RepoDescribe)
	serveMeta(t, nc, SubjectRepoList, svc.RepoList)
	serveMeta(t, nc, SubjectTagPut, svc.TagPut)
	serveMeta(t, nc, SubjectTagGet, svc.TagGet)
	serveMeta(t, nc, SubjectTagList, svc.TagList)
	serveMeta(t, nc, SubjectTagDelete, svc.TagDelete)
	serveMeta(t, nc, SubjectManifestPut, svc.ManifestPut)
	serveMeta(t, nc, SubjectManifestDescribe, svc.ManifestDescribe)
	serveMeta(t, nc, SubjectUploadCreate, svc.UploadCreate)
	serveMeta(t, nc, SubjectUploadGet, svc.UploadGet)
	serveMeta(t, nc, SubjectUploadUpdate, svc.UploadUpdate)
	serveMeta(t, nc, SubjectUploadDelete, svc.UploadDelete)

	var client MetaStore = NewNATSMetaStore(nc)
	const acct = svcTestAccount

	// Repo not-found round-trips as ErrNotFound.
	_, err := client.GetRepo(acct, "ghost")
	assert.ErrorIs(t, err, ErrNotFound)

	require.NoError(t, client.PutRepo(acct, RepoMeta{Name: "team/app", CreatedAt: time.Now()}))
	got, err := client.GetRepo(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, "team/app", got.Name)
	repos, err := client.ListRepos(acct)
	require.NoError(t, err)
	assert.Equal(t, []string{"team/app"}, repos)

	// Tags.
	require.NoError(t, client.PutTag(acct, "team/app", "v1", "sha256:aaa"))
	d, err := client.GetTag(acct, "team/app", "v1")
	require.NoError(t, err)
	assert.Equal(t, "sha256:aaa", d)
	tags, err := client.ListTags(acct, "team/app")
	require.NoError(t, err)
	assert.Equal(t, []string{"v1"}, tags)
	require.NoError(t, client.DeleteTag(acct, "team/app", "v1"))
	assert.ErrorIs(t, client.DeleteTag(acct, "team/app", "v1"), ErrNotFound)

	// Manifest meta.
	require.NoError(t, client.PutManifestMeta(acct, "team/app", ManifestMeta{Digest: "sha256:ccc", MediaType: "x", Size: 9}))
	mm, err := client.GetManifestMeta(acct, "team/app", "sha256:ccc")
	require.NoError(t, err)
	assert.Equal(t, int64(9), mm.Size)
	_, err = client.GetManifestMeta(acct, "team/app", "sha256:zzz")
	assert.ErrorIs(t, err, ErrNotFound)

	// Upload CAS lifecycle round-trip, including the conflict mapping.
	rev, err := client.PutUpload(acct, "u1", UploadState{RepoName: "team/app"})
	require.NoError(t, err)
	st, gotRev, err := client.GetUpload(acct, "u1")
	require.NoError(t, err)
	assert.Equal(t, rev, gotRev)

	_, err = client.UpdateUpload(acct, "u1", st, gotRev+99)
	assert.ErrorIs(t, err, ErrConflict)

	st.CommittedBytes = 5
	newRev, err := client.UpdateUpload(acct, "u1", st, gotRev)
	require.NoError(t, err)
	assert.Greater(t, newRev, gotRev)

	require.NoError(t, client.DeleteUpload(acct, "u1"))
	_, _, err = client.GetUpload(acct, "u1")
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, client.DeleteUpload(acct, "u1"), ErrNotFound)
}
