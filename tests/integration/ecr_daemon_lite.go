//go:build integration

package integration

import (
	"testing"

	"github.com/mulgadc/spinifex/spinifex/domains/ecr"
	"github.com/nats-io/nats.go"
)

// StartECRDaemonLite subscribes a real ecr.MetaServiceImpl (the same
// production service the live daemon runs, see daemon/daemon.go)
// to every ecr.* NATS subject the gateway's ECR control-plane handlers and
// ECRRegistry.Meta dial for metadata — repos, policies, lifecycle policies,
// tags, manifest records, and in-progress upload state. It is memory-backed
// (ecr.NewMemoryMetaStore) rather than JetStream-KV-backed, matching
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
func StartECRDaemonLite(t *testing.T, gw *Gateway) *ecr.MetaServiceImpl {
	t.Helper()

	svc := ecr.NewMetaServiceImpl(ecr.NewMemoryMetaStore())
	nc := gw.NATSConn

	sub(t, nc, ecr.SubjectRepoCreate, func(m *nats.Msg) { dispatch(m, svc.RepoCreate) })
	sub(t, nc, ecr.SubjectRepoDescribe, func(m *nats.Msg) { dispatch(m, svc.RepoDescribe) })
	sub(t, nc, ecr.SubjectRepoList, func(m *nats.Msg) { dispatch(m, svc.RepoList) })
	sub(t, nc, ecr.SubjectRepoDelete, func(m *nats.Msg) { dispatch(m, svc.RepoDelete) })

	sub(t, nc, ecr.SubjectPolicyPut, func(m *nats.Msg) { dispatch(m, svc.PolicyPut) })
	sub(t, nc, ecr.SubjectPolicyGet, func(m *nats.Msg) { dispatch(m, svc.PolicyGet) })
	sub(t, nc, ecr.SubjectPolicyDelete, func(m *nats.Msg) { dispatch(m, svc.PolicyDelete) })

	sub(t, nc, ecr.SubjectLifecyclePut, func(m *nats.Msg) { dispatch(m, svc.LifecyclePut) })
	sub(t, nc, ecr.SubjectLifecycleGet, func(m *nats.Msg) { dispatch(m, svc.LifecycleGet) })
	sub(t, nc, ecr.SubjectLifecycleDelete, func(m *nats.Msg) { dispatch(m, svc.LifecycleDelete) })

	sub(t, nc, ecr.SubjectTagPut, func(m *nats.Msg) { dispatch(m, svc.TagPut) })
	sub(t, nc, ecr.SubjectTagGet, func(m *nats.Msg) { dispatch(m, svc.TagGet) })
	sub(t, nc, ecr.SubjectTagList, func(m *nats.Msg) { dispatch(m, svc.TagList) })
	sub(t, nc, ecr.SubjectTagDelete, func(m *nats.Msg) { dispatch(m, svc.TagDelete) })

	sub(t, nc, ecr.SubjectManifestPut, func(m *nats.Msg) { dispatch(m, svc.ManifestPut) })
	sub(t, nc, ecr.SubjectManifestDescribe, func(m *nats.Msg) { dispatch(m, svc.ManifestDescribe) })
	sub(t, nc, ecr.SubjectManifestList, func(m *nats.Msg) { dispatch(m, svc.ManifestList) })
	sub(t, nc, ecr.SubjectManifestDelete, func(m *nats.Msg) { dispatch(m, svc.ManifestDelete) })

	sub(t, nc, ecr.SubjectUploadCreate, func(m *nats.Msg) { dispatch(m, svc.UploadCreate) })
	sub(t, nc, ecr.SubjectUploadGet, func(m *nats.Msg) { dispatch(m, svc.UploadGet) })
	sub(t, nc, ecr.SubjectUploadUpdate, func(m *nats.Msg) { dispatch(m, svc.UploadUpdate) })
	sub(t, nc, ecr.SubjectUploadDelete, func(m *nats.Msg) { dispatch(m, svc.UploadDelete) })

	return svc
}
