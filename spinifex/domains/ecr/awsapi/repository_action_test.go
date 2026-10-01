package awsapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/internal/testkit"
	handlers_ecr "github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/mulgadc/spinifex/spinifex/utils"
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
	svc := handlers_ecr.NewKVMetaService(js)
	serveRepositoryActionMeta(t, nc, handlers_ecr.SubjectRepoCreate, svc.RepoCreate)
	serveRepositoryActionMeta(t, nc, handlers_ecr.SubjectRepoDescribe, svc.RepoDescribe)
	serveRepositoryActionMeta(t, nc, handlers_ecr.SubjectRepoDelete, svc.RepoDelete)
	serveRepositoryActionMeta(t, nc, handlers_ecr.SubjectRepoList, svc.RepoList)
	serveRepositoryActionMeta(t, nc, handlers_ecr.SubjectManifestPut, svc.ManifestPut)
	serveRepositoryActionMeta(t, nc, handlers_ecr.SubjectManifestList, svc.ManifestList)
	return nc
}

func serveRepositoryActionMeta[I any, O any](t *testing.T, nc *nats.Conn, subject string, fn func(context.Context, *I, string) (*O, error)) {
	t.Helper()
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		accountID := utils.AccountIDFromMsg(msg)
		in := new(I)
		if errResp := utils.UnmarshalJsonPayload(in, msg.Data); errResp != nil {
			_ = msg.Respond(errResp)
			return
		}
		out, err := fn(context.Background(), in, accountID)
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

func seedRepositoryForAction(t *testing.T, nc *nats.Conn, name string) {
	t.Helper()
	store := handlers_ecr.NewNATSMetaStore(nc)
	require.NoError(t, store.PutRepo(context.Background(), repositoryActionTestAccount, handlers_ecr.RepoMeta{
		Name: name, CreatedAt: time.Now(),
	}))
}

func repositoryActionEndpoint() RepositoryEndpoint {
	return RepositoryEndpoint{Region: "ap-southeast-2", ServicesDomain: "spinifex.test"}
}
