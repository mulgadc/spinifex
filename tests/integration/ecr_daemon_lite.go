//go:build integration

package integration

import (
	"testing"

	handlers_ecr "github.com/mulgadc/spinifex/spinifex/handlers/ecr"
	"github.com/nats-io/nats.go"
)

// StartECRDaemonLite subscribes a real handlers_ecr.MetaServiceImpl (the same
// production service the live daemon runs, see daemon/daemon_handlers_ecr.go)
// to every ecr.* NATS subject the gateway's ECR control-plane handlers and
// ECRRegistry.Meta dial for metadata — repos, policies, lifecycle policies,
// tags, manifest records, and in-progress upload state. It is memory-backed
// (handlers_ecr.NewMemoryMetaStore) rather than JetStream-KV-backed, matching
// DaemonLite's in-scope resources.
//
// Blob and manifest bytes never travel these subjects (ECRRegistry.Store
// handles those directly against its own objectstore.ObjectStore, wired in
// StartGateway); this only stands in for the daemon's metadata half.
//
// Must be called before a test issues any ECR request beyond
// GetAuthorizationToken: StartGateway wires ECRRegistry.Meta as a NATS client
// with no responder until this runs, so an early call times out after 30s
// rather than failing fast.
func StartECRDaemonLite(t *testing.T, gw *Gateway) *handlers_ecr.MetaServiceImpl {
	t.Helper()

	svc := handlers_ecr.NewMetaServiceImpl(handlers_ecr.NewMemoryMetaStore())
	nc := gw.NATSConn

	sub(t, nc, handlers_ecr.SubjectRepoCreate, func(m *nats.Msg) { dispatch(m, svc.RepoCreate) })
	sub(t, nc, handlers_ecr.SubjectRepoDescribe, func(m *nats.Msg) { dispatch(m, svc.RepoDescribe) })
	sub(t, nc, handlers_ecr.SubjectRepoList, func(m *nats.Msg) { dispatch(m, svc.RepoList) })
	sub(t, nc, handlers_ecr.SubjectRepoDelete, func(m *nats.Msg) { dispatch(m, svc.RepoDelete) })

	sub(t, nc, handlers_ecr.SubjectPolicyPut, func(m *nats.Msg) { dispatch(m, svc.PolicyPut) })
	sub(t, nc, handlers_ecr.SubjectPolicyGet, func(m *nats.Msg) { dispatch(m, svc.PolicyGet) })
	sub(t, nc, handlers_ecr.SubjectPolicyDelete, func(m *nats.Msg) { dispatch(m, svc.PolicyDelete) })

	sub(t, nc, handlers_ecr.SubjectLifecyclePut, func(m *nats.Msg) { dispatch(m, svc.LifecyclePut) })
	sub(t, nc, handlers_ecr.SubjectLifecycleGet, func(m *nats.Msg) { dispatch(m, svc.LifecycleGet) })
	sub(t, nc, handlers_ecr.SubjectLifecycleDelete, func(m *nats.Msg) { dispatch(m, svc.LifecycleDelete) })

	sub(t, nc, handlers_ecr.SubjectTagPut, func(m *nats.Msg) { dispatch(m, svc.TagPut) })
	sub(t, nc, handlers_ecr.SubjectTagGet, func(m *nats.Msg) { dispatch(m, svc.TagGet) })
	sub(t, nc, handlers_ecr.SubjectTagList, func(m *nats.Msg) { dispatch(m, svc.TagList) })
	sub(t, nc, handlers_ecr.SubjectTagDelete, func(m *nats.Msg) { dispatch(m, svc.TagDelete) })

	sub(t, nc, handlers_ecr.SubjectManifestPut, func(m *nats.Msg) { dispatch(m, svc.ManifestPut) })
	sub(t, nc, handlers_ecr.SubjectManifestDescribe, func(m *nats.Msg) { dispatch(m, svc.ManifestDescribe) })
	sub(t, nc, handlers_ecr.SubjectManifestList, func(m *nats.Msg) { dispatch(m, svc.ManifestList) })
	sub(t, nc, handlers_ecr.SubjectManifestDelete, func(m *nats.Msg) { dispatch(m, svc.ManifestDelete) })

	sub(t, nc, handlers_ecr.SubjectUploadCreate, func(m *nats.Msg) { dispatch(m, svc.UploadCreate) })
	sub(t, nc, handlers_ecr.SubjectUploadGet, func(m *nats.Msg) { dispatch(m, svc.UploadGet) })
	sub(t, nc, handlers_ecr.SubjectUploadUpdate, func(m *nats.Msg) { dispatch(m, svc.UploadUpdate) })
	sub(t, nc, handlers_ecr.SubjectUploadDelete, func(m *nats.Msg) { dispatch(m, svc.UploadDelete) })

	return svc
}
