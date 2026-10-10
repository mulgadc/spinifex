package awsapi

import (
	"context"
	"encoding/json"
	"github.com/mulgadc/spinifex/spinifex/foundation/aws/errors"
	"github.com/mulgadc/spinifex/spinifex/foundation/messaging/nats"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// repositoryActionTestAccount is the account served by the direct repository
// AWS-action tests. It is intentionally separate from gateway auth tests.
const repositoryActionTestAccount = "000000000000"

// newRepositoryActionTestConn provides only the metadata subjects needed by
// repository actions. It keeps those action tests independent of gateway HTTP
// construction while exercising the deployed NATS request contract.
func newRepositoryActionTestConn(t *testing.T) *nats.Conn {
	t.Helper()
	_, nc, _ := testutil.StartTestJetStream(t)
	js := testutil.NewJetStream(t, nc)
	svc := ecr.NewKVMetaService(js)
	serveRepositoryActionMeta(t, nc, ecr.SubjectRepoCreate, svc.RepoCreate)
	serveRepositoryActionMeta(t, nc, ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveRepositoryActionMeta(t, nc, ecr.SubjectRepoDelete, svc.RepoDelete)
	serveRepositoryActionMeta(t, nc, ecr.SubjectRepoList, svc.RepoList)
	serveRepositoryActionMeta(t, nc, ecr.SubjectManifestPut, svc.ManifestPut)
	serveRepositoryActionMeta(t, nc, ecr.SubjectManifestList, svc.ManifestList)
	return nc
}

func serveRepositoryActionMeta[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		accountID := natsmsg.AccountIDFromMsg(msg)
		in := new(I)
		if errResp := awserrors.UnmarshalJsonPayload(in, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		out, err := fn(context.Background(), in, accountID)
		if err != nil {
			_ = msg.Respond(awserrors.GenerateErrorPayload("ServerInternal"))
			return
		}
		data, _ := json.Marshal(out)
		_ = msg.Respond(data)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = sub.Unsubscribe() })
}

func seedRepositoryForAction(t *testing.T, nc *nats.Conn, name string) {
	t.Helper()
	store := ecr.NewNATSMetaStore(nc)
	require.NoError(t, store.PutRepo(context.Background(), repositoryActionTestAccount, ecr.RepoMeta{
		Name: name, CreatedAt: time.Now(),
	}))
}

func repositoryActionEndpoint() RepositoryEndpoint {
	return RepositoryEndpoint{Region: "ap-southeast-2", ServicesDomain: "spinifex.test"}
}
